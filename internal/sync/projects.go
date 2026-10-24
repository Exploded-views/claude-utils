package sync

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"time"
)

// ProjectEntry is one project's record in the per-PC cache. All fields are
// optional and meant to be edited by hand:
//
//   - Dir pins the local directory this folder name stands for.
//   - Key overrides the repo-side key outright (a value like
//     "github.com/acme/app"), or "path" to force the path key.
//   - LastKey is the key the daemon last resolved for this folder. It is
//     kept while the directory is gone or git cannot answer.
//   - Candidates is written by the daemon when the folder name decodes to
//     several directories; set Dir to the right one.
type ProjectEntry struct {
	Dir        string   `json:"dir,omitempty"`
	Key        string   `json:"key,omitempty"`
	LastKey    string   `json:"lastKey,omitempty"`
	Candidates []string `json:"candidates,omitempty"`
}

// ProjectCache maps Claude project folder names to what the daemon worked out
// about them. It lives in .state/, which is git-ignored, so it is per-PC. Stop
// the daemon before editing the file: it rewrites it as resolution changes.
type ProjectCache struct {
	Version  int                     `json:"version"`
	Projects map[string]ProjectEntry `json:"projects"`

	readOnly bool // the file could not be read: never write over it
}

const projectCacheVersion = 1

// ProjectCachePath is the cache file's location inside syncDir.
func ProjectCachePath(syncDir string) string {
	return filepath.Join(syncDir, manifestStateDir, "projects.json")
}

// LoadProjectCache reads the cache. A missing file yields an empty cache. A
// file that does not parse, or carries a version this build does not know, is
// renamed to projects.json.corrupt-<unix time> and also yields an empty cache:
// the cache only records what can be re-derived, so a torn, hand-mangled or
// newer file must never stop the daemon. A UTF-8 byte order mark, which some
// Windows editors add, is ignored. Any other read error (permission denied, a
// directory in the file's place) is logged and gives an empty in-memory cache
// that is never saved, so the file is left as it is.
//
// The daemon rewrites the file whenever resolution changes, so stop it before
// editing the file by hand.
func LoadProjectCache(syncDir string) (*ProjectCache, error) {
	c := &ProjectCache{Version: projectCacheVersion, Projects: map[string]ProjectEntry{}}
	p := ProjectCachePath(syncDir)
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		log.Printf("remote keys: %v; carrying on with an empty cache and leaving the file alone", err)
		c.readOnly = true
		return c, nil
	}
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	err = json.Unmarshal(b, c)
	if err == nil && c.Version != 0 && c.Version != projectCacheVersion {
		err = fmt.Errorf("unknown version %d", c.Version)
	}
	if err != nil {
		aside := fmt.Sprintf("%s.corrupt-%d", p, time.Now().Unix())
		log.Printf("remote keys: %s: %v; moving it to %s and starting with an empty cache", p, err, aside)
		if err := os.Rename(p, aside); err != nil {
			log.Println("remote keys: set aside corrupt cache:", err)
		}
		return &ProjectCache{Version: projectCacheVersion, Projects: map[string]ProjectEntry{}}, nil
	}
	if c.Projects == nil {
		c.Projects = map[string]ProjectEntry{}
	}
	return c, nil
}

// Save writes the cache as pretty-printed JSON, atomically.
func (c *ProjectCache) Save(syncDir string) error {
	if c.readOnly {
		return nil
	}
	c.Version = projectCacheVersion
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.MkdirAll(filepath.Dir(ProjectCachePath(syncDir)), 0700); err != nil {
		return err
	}
	return writeAtomic(ProjectCachePath(syncDir), b)
}

// logFolder is how every log line about one project folder is written. It
// prints the Claude folder name and keys, never a directory, and callers pass
// nothing else: that way the lines can be redacted in this one place.
func logFolder(folder, format string, args ...any) {
	log.Printf("remote keys: %s: "+format, append([]any{folder}, args...)...)
}

// RemoteMap decides, for each local Claude project folder, whether it is
// stored under a remote key and which one.
//
// A remote key has at most one folder per PC. The first folder to claim a key
// keeps it, and the claim is recorded in the cache (the folder's lastKey), so
// it survives restarts and a vanished checkout. Any other folder that resolves
// to a key somebody else holds uses its path key, exactly as in path mode.
type RemoteMap struct {
	syncDir    string
	claudeRoot string
	// Lookup returns the origin URL for a project directory, "" if it has
	// none, and false if git could not tell. Replaceable in tests.
	Lookup func(dir string) (url string, ok bool)
	// Why names the class of failure (see classifyGitFailure) after Lookup
	// reported false. Replaceable in tests.
	Why func(dir string) string

	cache  *ProjectCache
	keys   map[string]string // folder name -> key; "" means path key
	owner  map[string]string // key -> the folder that holds it on this PC
	warned map[string]bool   // invalid overrides already logged, by folder name
	failed map[string]bool   // folders already logged as unresolvable, by folder name
	denied map[string]bool   // folders already logged as refused a key, by folder name
	dirty  bool              // cache changed since it was last saved
}

// NewRemoteMap loads the per-PC cache and resolves every project folder.
func NewRemoteMap(syncDir, claudeRoot string) (*RemoteMap, error) {
	c, err := LoadProjectCache(syncDir)
	if err != nil {
		return nil, err
	}
	m := &RemoteMap{syncDir: syncDir, claudeRoot: claudeRoot, Lookup: gitOrigin, Why: gitFailure, cache: c,
		keys: map[string]string{}, owner: map[string]string{}, warned: map[string]bool{}, failed: map[string]bool{}, denied: map[string]bool{}}
	m.Refresh()
	return m, nil
}

// Refresh re-resolves every project folder under the Claude root and saves
// the cache if anything changed.
func (m *RemoteMap) Refresh() {
	m.keys = map[string]string{}
	m.owner = map[string]string{}
	// Keys claimed on an earlier run stay claimed, whether or not the folder
	// is looked at first (or at all) this time.
	held := make([]string, 0, len(m.cache.Projects))
	for h := range m.cache.Projects {
		held = append(held, h)
	}
	sort.Strings(held)
	for _, h := range held {
		if k := lastKey(m.cache.Projects[h]); k != "" {
			if _, taken := m.owner[k]; !taken {
				m.owner[k] = h
			}
		}
	}
	entries, err := os.ReadDir(m.claudeRoot)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Println("remote keys: read", m.claudeRoot+":", err)
	}
	for _, e := range entries { // sorted by name
		if e.IsDir() {
			m.resolve(e.Name())
		}
	}
	m.save()
}

// Key returns the repo-side key for a project folder, or "" for the path
// key. A folder not seen before is resolved on demand.
func (m *RemoteMap) Key(hash string) string {
	if k, ok := m.keys[hash]; ok {
		return k
	}
	if !isDir(filepath.Join(m.claudeRoot, hash)) {
		return "" // not a project folder on this PC (e.g. a hash from another PC's push)
	}
	k := m.resolve(hash)
	m.save()
	return k
}

// MemoryDir is the repo-side memory directory for a remote key.
func (m *RemoteMap) MemoryDir(key string) string {
	return filepath.Join(m.syncDir, remotesDir, filepath.FromSlash(key), "memory")
}

// FolderFor returns the local project folder that holds key on this PC.
func (m *RemoteMap) FolderFor(key string) (hash string, ok bool) {
	for h, k := range m.keys {
		if k == key {
			return h, true
		}
	}
	return "", false
}

// keyOf resolves dir's key; ok is false if git could not tell.
func (m *RemoteMap) keyOf(dir string) (key string, ok bool) {
	url, ok := m.Lookup(dir)
	if !ok {
		return "", false
	}
	k, _ := NormalizeRemote(url)
	return k, true
}

// lastKey is the key to keep using when nothing positive is known.
func lastKey(e ProjectEntry) string {
	if validKey(e.LastKey) {
		return e.LastKey
	}
	return ""
}

// resolve works out hash's key and records it. See wantedKey for the evidence
// rules and claim for the one-member-per-key rule.
func (m *RemoteMap) resolve(hash string) string {
	prev, had := m.cache.Projects[hash]
	e := prev
	key := m.claim(hash, m.wantedKey(hash, &e))
	e.LastKey = key

	m.keys[hash] = key
	switch {
	case reflect.DeepEqual(e, ProjectEntry{}):
		if had {
			delete(m.cache.Projects, hash)
			m.dirty = true
		}
	case !had || !reflect.DeepEqual(e, prev):
		m.cache.Projects[hash] = e
		m.dirty = true
	}
	return key
}

// claim grants want to hash unless another folder holds it, and releases
// whatever hash held before. "" (the path key) is always granted.
func (m *RemoteMap) claim(hash, want string) string {
	for k, h := range m.owner {
		if h == hash && k != want {
			delete(m.owner, k)
		}
	}
	if want == "" {
		return ""
	}
	if h, taken := m.owner[want]; taken && h != hash {
		if !m.denied[hash] {
			m.denied[hash] = true
			logFolder(hash, "would be stored as remotes/%s, which %s already holds on this PC; keeping its path key",
				want, h)
		}
		return ""
	}
	m.owner[want] = hash
	return want
}

// wantedKey is the key hash should have, whoever else holds it. It updates e's
// Dir and Candidates; the caller records the key itself.
func (m *RemoteMap) wantedKey(hash string, e *ProjectEntry) string {
	override := e.Key // an invalid one is ignored but left in the file for the user to fix
	if override != "" && override != "path" && !validKey(override) {
		if !m.warned[hash] {
			m.warned[hash] = true
			log.Printf("remote keys: ignoring invalid \"key\" %q for %s in %s (want host/owner/repo of [a-z0-9._-])",
				override, hash, ProjectCachePath(m.syncDir))
		}
		override = ""
	}
	switch override {
	case "path":
		return ""
	case "":
	default:
		return override
	}

	// Keys are sticky: a deleted checkout, an unmounted drive or a git
	// that will not answer must not flip a folder back to its path key
	// and fork its notes. Only git saying "no origin" or a different
	// origin changes the key.
	key := lastKey(*e)
	recorded := e.Dir
	dir := e.Dir
	if dir != "" {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			dir = "" // moved or deleted since last time
		}
	}
	prevCandidates := e.Candidates
	e.Candidates = nil
	if dir == "" {
		cands := Candidates(hash)
		switch len(cands) {
		case 0:
		case 1:
			dir = cands[0]
		default:
			// Several directories escape to this name. If they all agree
			// on the remote it does not matter which one we pick;
			// otherwise guess nothing and let the user choose.
			keys := map[string]bool{}
			known := true
			for _, c := range cands {
				k, ok := m.keyOf(c)
				known = known && ok
				keys[k] = true
			}
			if known && len(keys) == 1 && !keys[""] {
				dir = cands[0]
			} else {
				// e.Dir stays: it is only used after a stat, and a pinned
				// dir (or the last good one) is worth keeping through a
				// spell where the name is ambiguous.
				e.Candidates = cands
				if !reflect.DeepEqual(prevCandidates, cands) {
					logFolder(hash, "matches %d directories with different remotes; keeping its previous key (set \"dir\" in .state/projects.json to choose)",
						len(cands))
				}
			}
		}
	}
	k, ok := "", false
	if dir != "" {
		k, ok = m.keyOf(dir)
	}
	switch {
	case dir == "":
	case recorded != "" && dir != recorded && !(ok && k != ""):
		// The recorded directory is gone and this is a different one that
		// escapes to the same name. Failing to answer or having no origin
		// says nothing about the project that used to be at recorded, so
		// only a real origin may re-key the folder.
	default:
		e.Dir = dir
		if ok {
			key = k
		} else if key == "" && !m.failed[hash] {
			// Nothing remembered and git cannot answer: the folder gets
			// its path key, which forks its notes from other PCs until
			// this is fixed. Say so once, without the path.
			m.failed[hash] = true
			logFolder(hash, "cannot read the git origin (%s); using its path key until git answers", m.Why(dir))
		}
	}
	return key
}

// save writes the cache if resolution changed it.
func (m *RemoteMap) save() {
	if !m.dirty {
		return
	}
	if err := m.cache.Save(m.syncDir); err != nil {
		log.Println("remote keys: save cache:", err) // still dirty: the next save retries
		return
	}
	m.dirty = false
}
