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
	"strings"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// ProjectEntry is one project's record in the per-PC cache. All fields are
// optional and meant to be edited by hand:
//
//   - Dir pins the local directory this folder name stands for.
//   - Key overrides the repo-side key outright (a value like
//     "github.com/acme/app"), or "path" to force the path key.
//   - LastKey is the key the daemon last resolved for this folder: the claim
//     on that key, not just a note. It is kept while the directory is gone or
//     git cannot answer, and editing it moves the claim (stop the daemon
//     first).
//   - Candidates is written by the daemon when the folder name decodes to
//     several directories; set Dir to the right one.
//   - LeftAt and JoinPending are written by the daemon (see the fields).
type ProjectEntry struct {
	Dir        string   `json:"dir,omitempty"`
	Key        string   `json:"key,omitempty"`
	LastKey    string   `json:"lastKey,omitempty"`
	Candidates []string `json:"candidates,omitempty"`
	// LeftAt records, per repo directory this folder used to be stored in
	// ("projects/<hash>/memory", "remotes/<key>/memory"), the sync repo's HEAD
	// when the folder left it ("" if the repo had no commit: everything in the
	// directory then counts as new). A file only the repo has in a directory the
	// folder comes back to is imported only if it changed since. An anchor that
	// no longer resolves imports nothing. The entry for a remote directory goes
	// when the folder re-joins it (see joined). Written by the daemon.
	LeftAt map[string]string `json:"leftAt,omitempty"`
	// JoinPending is set when the folder gains or changes its key and cleared
	// once it has been joined to that key's notes (see Joining a key in the
	// docs), so a crash in between still joins on the next start. Written by
	// the daemon.
	JoinPending bool `json:"joinPending,omitempty"`
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
// prints the Claude folder name and keys, and callers pass nothing else (no
// directory, no URL). The folder name is not free of paths: Claude derives it
// from the project's absolute path, every character that is not a letter or
// digit turned into '-', so it reveals that path in escaped form. Keeping the
// formatting in this one place is what lets the lines be redacted or
// changed together.
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

	restored map[string]bool // keys whose vanished holder's notes were restored from HEAD, until the holder's folder returns
	join     map[string]bool // folders that gained their key and are not joined to its notes yet

	changed    map[string]map[string]bool // "anchor dir" -> paths git says changed since (nil: unknown)
	diffFailed map[string]bool            // folders already logged as unable to compare, by folder name
}

// NewRemoteMap loads the per-PC cache and resolves every project folder.
func NewRemoteMap(syncDir, claudeRoot string) (*RemoteMap, error) {
	c, err := LoadProjectCache(syncDir)
	if err != nil {
		return nil, err
	}
	m := newRemoteMap(syncDir, claudeRoot, c)
	for h, e := range c.Projects {
		if e.JoinPending {
			m.join[h] = true // claimed on an earlier run, never joined
		}
	}
	m.Refresh()
	return m, nil
}

func newRemoteMap(syncDir, claudeRoot string, c *ProjectCache) *RemoteMap {
	return &RemoteMap{syncDir: syncDir, claudeRoot: claudeRoot, Lookup: gitOrigin, Why: gitFailure, cache: c,
		keys: map[string]string{}, owner: map[string]string{}, warned: map[string]bool{}, failed: map[string]bool{}, denied: map[string]bool{}, restored: map[string]bool{}, join: map[string]bool{},
		changed: map[string]map[string]bool{}, diffFailed: map[string]bool{}}
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
		e := m.cache.Projects[h]
		if k := lastKey(e); k != "" {
			if m.staleClaim(h, e) {
				m.release(h, k)
				continue
			}
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

// staleClaim reports whether nothing is left of the folder holding a claim: its
// Claude folder and its recorded checkout are both gone (not merely
// unreachable: only "does not exist" counts), and no directory still decodes
// from its name. Anything less, say a Claude folder that is still there, keeps
// the claim; it is then moved by hand (see claim).
func (m *RemoteMap) staleClaim(hash string, e ProjectEntry) bool {
	if !gone(filepath.Join(m.claudeRoot, hash)) {
		return false
	}
	if e.Dir != "" && !gone(e.Dir) {
		return false
	}
	return len(Candidates(hash)) == 0
}

// gone reports whether p does not exist (as opposed to being unreadable).
func gone(p string) bool {
	_, err := os.Lstat(p)
	return errors.Is(err, fs.ErrNotExist)
}

// claimedKey is the key hash holds a claim on, whether or not its Claude folder
// still exists; "" if it holds none.
func (m *RemoteMap) claimedKey(hash string) string {
	for k, h := range m.owner {
		if h == hash {
			return k
		}
	}
	return ""
}

// Vanished lists the remote keys (sorted) whose holder's Claude project folder
// is gone while the claim is still held, and whose notes Restored has not been
// told about since the folder went. The notes under such a key are intact in
// the sync repo and must stay that way, whatever deletes the vanished folder's
// files looked like to the watcher. (The notes of a claim that is released are
// restored by the release itself, see release.)
func (m *RemoteMap) Vanished() []string {
	var out []string
	for k, h := range m.owner {
		if isDir(filepath.Join(m.claudeRoot, h)) {
			delete(m.restored, k) // back again: a later disappearance is a new one
		} else if !m.restored[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Restored records that the notes of key, which Vanished reported, have been
// restored, so that the next flush does not do it again.
func (m *RemoteMap) Restored(key string) { m.restored[key] = true }

// ReleaseStale drops every claim that staleClaim says is stale. Each release
// restores the key's notes (see release).
func (m *RemoteMap) ReleaseStale() {
	for _, k := range sortedKeys(m.owner) {
		h := m.owner[k]
		if m.staleClaim(h, m.cache.Projects[h]) {
			delete(m.owner, k)
			m.release(h, k)
		}
	}
	m.save()
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// release drops a stale claim and its cache entry, and puts back the key's
// notes from HEAD: the holder's files may have gone out of the working tree as
// deletes before its folder did. It happens here, before anyone can claim the
// key (a new holder's join writes into the same directory, and a restore after
// it would throw the join away), and not at all if a live folder holds the key.
func (m *RemoteMap) release(hash, key string) {
	delete(m.cache.Projects, hash)
	delete(m.join, hash)
	delete(m.restored, key)
	m.dirty = true
	logFolder(hash, "claim on remotes/%s released: its Claude folder and checkout are gone", key)
	if m.liveHolder(key) {
		return
	}
	m.restore(gitwt.New(m.syncDir), key)
}

// liveHolder reports whether a folder whose Claude folder exists holds key.
func (m *RemoteMap) liveHolder(key string) bool {
	if h, ok := m.owner[key]; ok && isDir(filepath.Join(m.claudeRoot, h)) {
		return true
	}
	for h, e := range m.cache.Projects {
		if lastKey(e) == key && isDir(filepath.Join(m.claudeRoot, h)) {
			return true
		}
	}
	return false
}

// restore checks the notes of key out from HEAD in repo (the sync repo). A repo
// with no commit has nothing to restore.
func (m *RemoteMap) restore(repo *gitwt.Repo, key string) {
	if m.head() == "" {
		return
	}
	dir := repoDir(key, "")
	if _, err := repo.Run("checkout", "HEAD", "--", dir); err != nil && !strings.Contains(err.Error(), "did not match") {
		logFolder(key, "cannot restore %s from HEAD after its Claude folder went away (%s)", dir, classifyGitFailure(err))
	}
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

// joined records that the folder's notes have been merged with its key's, and
// saves that, so a restart does not join it again. The folder's LeftAt entry for
// the key's directory goes with it, unless the join held back a file that only
// the repo had (refused): such a file is still indistinguishable from a note the
// folder deleted, and would be imported by the next reconcile once the entry
// is gone.
func (m *RemoteMap) joined(hash string, refused bool) {
	delete(m.join, hash)
	e, ok := m.cache.Projects[hash]
	if !ok {
		return
	}
	changed := e.JoinPending
	e.JoinPending = false
	if dir := repoDir(lastKey(e), hash); !refused {
		if _, left := e.LeftAt[dir]; left {
			out := make(map[string]string, len(e.LeftAt))
			for k, v := range e.LeftAt {
				if k != dir {
					out[k] = v
				}
			}
			if len(out) == 0 {
				out = nil
			}
			e.LeftAt = out
			changed = true
		}
	}
	if changed {
		m.cache.Projects[hash] = e
		m.dirty = true
		m.save()
	}
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
	if old := lastKey(prev); key != old {
		logFolder(hash, "key changed from %s to %s", keyLabel(old), keyLabel(key))
		e.LeftAt = m.leave(e.LeftAt, repoDir(old, hash), repoDir(key, hash))
	}
	if key != "" && key != lastKey(prev) {
		m.join[hash] = true // a new claim, a key change or a moved claim
		e.JoinPending = true
	} else if key == "" {
		delete(m.join, hash)
		e.JoinPending = false
	}
	e.LastKey = key
	if key != "" {
		delete(m.restored, key) // its folder is here: a later disappearance is a new one
	}

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

// keyLabel is a key as it is written in log lines.
func keyLabel(k string) string {
	if k == "" {
		return "path"
	}
	return k
}

// repoDir is the memory directory, relative to the sync repo, that a folder is
// stored in under key ("" for the path key).
func repoDir(key, hash string) string {
	if key == "" {
		return remotesPathDir(hash)
	}
	return remotesDir + "/" + key + "/memory"
}

func remotesPathDir(hash string) string { return "projects/" + hash + "/memory" }

// leave notes in left (a copy, so the cache entry it came from is not changed
// behind its back) that the folder stopped using from, at the sync repo's
// current HEAD.
func (m *RemoteMap) leave(left map[string]string, from, to string) map[string]string {
	if from == to {
		return left
	}
	out := make(map[string]string, len(left)+1)
	for k, v := range left {
		out[k] = v
	}
	out[from] = m.head()
	return out
}

// head is the sync repo's HEAD commit, or "" if that cannot be read (not a
// repo, no commit yet).
func (m *RemoteMap) head() string {
	out, err := gitwt.New(m.syncDir).Run("rev-parse", "--verify", "--quiet", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// mayImport reports whether a file that only the repo has, in the directory dir
// that hash is stored in, may be copied into the folder. A folder that never
// left dir imports anything: that is a note from another PC. One that came back
// to dir imports only what changed since it left; the rest cannot be told
// from a copy that was deleted here while the folder was elsewhere. An anchor
// of "" (the repo had no commit when it left) means everything is new. When git
// cannot say (the anchor no longer resolves, say because it was garbage
// collected, or git fails), nothing is imported and the daemon logs why once.
func (m *RemoteMap) mayImport(hash, dir, name string) bool {
	anchor, left := m.cache.Projects[hash].LeftAt[dir]
	if !left {
		return true
	}
	if anchor == "" {
		return true // the repo had no commit yet: nothing could have been deleted
	}
	set, ok := m.changed[anchor+" "+dir]
	if !ok {
		out, err := gitwt.New(m.syncDir).Run("diff", "--name-only", "-z", anchor+"..HEAD", "--", dir)
		if err != nil {
			if !m.diffFailed[hash] {
				m.diffFailed[hash] = true
				logFolder(hash, "cannot tell what changed in %s since the folder left it (%s); not importing files that only the repo has there",
					dir, classifyGitFailure(err))
			}
			set = nil
		} else {
			set = map[string]bool{}
			for _, p := range strings.Split(out, "\x00") {
				if p != "" {
					set[p] = true
				}
			}
		}
		m.changed[anchor+" "+dir] = set
	}
	return set[dir+"/"+name]
}

// forgetChanges drops the repo comparisons mayImport has cached; call it
// before a pass that should see the repo as it is now.
func (m *RemoteMap) forgetChanges() { m.changed = map[string]map[string]bool{} }

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
			if why := m.holderGone(h); why != "" {
				logFolder(hash, "would be stored as remotes/%s, which %s holds on this PC, but that folder's %s; "+
					"a claim never moves by itself. To move it, stop the daemon and in .state/projects.json set \"key\": \"path\" on %s "+
					"and \"key\": \"%s\" on %s; keeping the path key meanwhile",
					want, h, why, h, want, hash)
			} else {
				logFolder(hash, "would be stored as remotes/%s, which %s already holds on this PC; keeping its path key",
					want, h)
			}
		}
		return ""
	}
	m.owner[want] = hash
	return want
}

// holderGone says what is missing of the folder h holding a claim: "checkout is
// gone", "Claude folder is gone", or "" if both are there.
func (m *RemoteMap) holderGone(h string) string {
	if e := m.cache.Projects[h]; e.Dir != "" && gone(e.Dir) {
		return "checkout is gone"
	}
	if !isDir(filepath.Join(m.claudeRoot, h)) {
		return "Claude folder is gone"
	}
	return ""
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
