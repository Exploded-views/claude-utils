package sync

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// Remote-keyed projects ("projectKey": "remote").
//
// By default a project's repo-side folder is the escaped path Claude Code uses
// for its local folder, so the same repo checked out at two different paths is
// two unrelated projects. In remote mode a project whose directory is a git
// checkout with an `origin` remote is stored under
//
//	remotes/<host>/<owner>/<repo>/memory/
//
// in the sync repo, whatever the local path is. `remotes/` is a sibling of
// `projects/` rather than a subfolder of it so that the two layouts can never
// be confused with each other (a path key is exactly one segment deep; a
// remote key is at least two) and so that older daemons, which only look
// inside `projects/`, simply ignore it. Projects with no usable remote keep
// their path key under `projects/`.

const remotesDir = "remotes"

// EscapeProjectPath mirrors how Claude Code names a project folder: every
// character that is not an ASCII letter or digit becomes '-'. The mapping is
// lossy ('/', '.', '_', ' ' and '-' all give '-'), which is why going the
// other way (see Candidates) can be ambiguous.
func EscapeProjectPath(p string) string {
	var b strings.Builder
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

var scpLike = regexp.MustCompile(`^(?:[^@/\\]+@)?([^:/\\]+):(.+)$`)

// NormalizeRemote turns a git remote URL into a repo-side key of the form
// host/owner/repo (more segments for hosts that nest groups). The same
// repository reached over ssh or https, with or without ".git", credentials,
// a port, or different letter case, yields the same key. It reports false for
// anything that is not a stable identity across machines: local paths,
// file:// URLs, or URLs without at least an owner and a repo.
func NormalizeRemote(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	var host, path string
	switch {
	case strings.Contains(raw, "://"):
		u, err := url.Parse(raw)
		if err != nil || strings.EqualFold(u.Scheme, "file") {
			return "", false
		}
		host, path = u.Hostname(), u.Path
	case len(raw) > 1 && raw[1] == ':' && isLetter(raw[0]):
		return "", false // Windows drive path, not scp syntax
	default:
		m := scpLike.FindStringSubmatch(raw)
		if m == nil {
			return "", false
		}
		host, path = m[1], m[2]
	}
	host = sanitizeSegment(strings.ToLower(host))
	if host == "" {
		return "", false
	}
	path = strings.TrimSuffix(strings.Trim(strings.ToLower(path), "/"), ".git")
	segs := strings.Split(path, "/")
	if len(segs) < 2 {
		return "", false
	}
	for i, s := range segs {
		if s == "" || s == "." || s == ".." {
			return "", false
		}
		segs[i] = sanitizeSegment(s)
	}
	return host + "/" + strings.Join(segs, "/"), true
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// sanitizeSegment keeps a path segment safe to use as a directory name on
// every platform.
func sanitizeSegment(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

var windowsDrive = regexp.MustCompile(`^([A-Za-z])--(.+)$`)

// Candidates returns every existing directory whose escaped path equals the
// given project folder name. Usually that is exactly one. The escaping is
// lossy, so it can be several (src/my-app, src/my.app and src/my/app all
// escape alike) or none (the project was deleted or lives on another
// machine). Results are sorted.
func Candidates(escaped string) []string {
	escaped = EscapeProjectPath(escaped) // tolerate '_' etc. kept by other versions
	if m := windowsDrive.FindStringSubmatch(escaped); m != nil {
		return walkCandidates(m[1]+`:\`, m[2])
	}
	if strings.HasPrefix(escaped, "-") && len(escaped) > 1 {
		return walkCandidates(string(filepath.Separator), escaped[1:])
	}
	return nil
}

// walkCandidates finds the directories below root whose path, escaped,
// equals rest. It only descends into entries that can still match, so the
// cost is proportional to the siblings along the one real path.
func walkCandidates(root, rest string) []string {
	var out []string
	var walk func(dir, rest string)
	walk = func(dir, rest string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !isDirEntry(dir, e) {
				continue
			}
			n := EscapeProjectPath(e.Name())
			switch {
			case n == rest:
				out = append(out, filepath.Join(dir, e.Name()))
			case strings.HasPrefix(rest, n+"-") && len(rest) > len(n)+1:
				walk(filepath.Join(dir, e.Name()), rest[len(n)+1:])
			}
		}
	}
	walk(root, rest)
	sort.Strings(out)
	return out
}

// isDirEntry reports whether e is a directory, following symlinks.
func isDirEntry(parent string, e fs.DirEntry) bool {
	if e.IsDir() {
		return true
	}
	if e.Type()&fs.ModeSymlink != 0 {
		info, err := os.Stat(filepath.Join(parent, e.Name()))
		return err == nil && info.IsDir()
	}
	return false
}

// gitOrigin returns the origin URL of dir if dir is the top of a git
// work-tree (a plain clone or a linked worktree), else "". A subdirectory of a
// repo is deliberately not treated as the repo: two different subprojects
// would otherwise share one memory folder.
func gitOrigin(dir string) string {
	r := gitwt.New(dir)
	top, err := r.Run("rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	ti, err1 := os.Stat(strings.TrimSpace(top))
	di, err2 := os.Stat(dir)
	if err1 != nil || err2 != nil || !os.SameFile(ti, di) {
		return ""
	}
	out, err := r.Run("config", "--local", "--get", "remote.origin.url")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// ProjectEntry is one project's record in the per-PC cache. All fields are
// optional and meant to be edited by hand:
//
//   - Dir pins the local directory this folder name stands for.
//   - Key overrides the repo-side key outright (a value like
//     "github.com/acme/app"), or "path" to force the path key.
//   - Candidates is written by the daemon when the folder name decodes to
//     several directories; set Dir to the right one.
type ProjectEntry struct {
	Dir        string   `json:"dir,omitempty"`
	Key        string   `json:"key,omitempty"`
	Candidates []string `json:"candidates,omitempty"`
}

// ProjectCache maps Claude project folder names to what the daemon worked out
// about them. It lives in .state/, which is git-ignored, so it is per-PC.
type ProjectCache struct {
	Version  int                     `json:"version"`
	Projects map[string]ProjectEntry `json:"projects"`
}

const projectCacheVersion = 1

// ProjectCachePath is the cache file's location inside syncDir.
func ProjectCachePath(syncDir string) string {
	return filepath.Join(syncDir, manifestStateDir, "projects.json")
}

// LoadProjectCache reads the cache. A missing file yields an empty cache. A
// file that does not parse is renamed to projects.json.corrupt-<unix time> and
// also yields an empty cache: the cache only records what can be re-derived,
// so a torn or hand-mangled file must never stop the daemon.
func LoadProjectCache(syncDir string) (*ProjectCache, error) {
	c := &ProjectCache{Version: projectCacheVersion, Projects: map[string]ProjectEntry{}}
	b, err := os.ReadFile(ProjectCachePath(syncDir))
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, c); err != nil {
		p := ProjectCachePath(syncDir)
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

// RemoteMap decides, for each local Claude project folder, whether it is
// stored under a remote key and which one.
type RemoteMap struct {
	syncDir    string
	claudeRoot string
	// Lookup returns the origin URL for a project directory, or "" if it has
	// none. Replaceable in tests.
	Lookup func(dir string) string

	cache *ProjectCache
	keys  map[string]string // folder name -> key; "" means path key
	dirty bool              // cache changed since it was last saved
}

// NewRemoteMap loads the per-PC cache and resolves every project folder.
func NewRemoteMap(syncDir, claudeRoot string) (*RemoteMap, error) {
	c, err := LoadProjectCache(syncDir)
	if err != nil {
		return nil, err
	}
	m := &RemoteMap{syncDir: syncDir, claudeRoot: claudeRoot, Lookup: gitOrigin, cache: c, keys: map[string]string{}}
	m.Refresh()
	return m, nil
}

// Refresh re-resolves every project folder under the Claude root and saves
// the cache if anything changed.
func (m *RemoteMap) Refresh() {
	m.keys = map[string]string{}
	entries, err := os.ReadDir(m.claudeRoot)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Println("remote keys: read", m.claudeRoot+":", err)
	}
	for _, e := range entries {
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
	k := m.resolve(hash)
	m.save()
	return k
}

// Members lists the known project folders stored under key, sorted.
func (m *RemoteMap) Members(key string) []string {
	var out []string
	for h, k := range m.keys {
		if k == key {
			out = append(out, h)
		}
	}
	sort.Strings(out)
	return out
}

// MemoryDir is the repo-side memory directory for a remote key.
func (m *RemoteMap) MemoryDir(key string) string {
	return filepath.Join(m.syncDir, remotesDir, filepath.FromSlash(key), "memory")
}

func (m *RemoteMap) keyOf(dir string) string {
	if k, ok := NormalizeRemote(m.Lookup(dir)); ok {
		return k
	}
	return ""
}

func (m *RemoteMap) resolve(hash string) string {
	prev, had := m.cache.Projects[hash]
	e := prev
	var key string

	switch {
	case e.Key == "path":
	case e.Key != "":
		key = e.Key
	default:
		dir := e.Dir
		if dir != "" {
			if info, err := os.Stat(dir); err != nil || !info.IsDir() {
				dir = "" // moved or deleted since last time
			}
		}
		e.Candidates = nil
		if dir == "" {
			e.Dir = ""
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
				for _, c := range cands {
					keys[m.keyOf(c)] = true
				}
				if len(keys) == 1 && !keys[""] {
					dir = cands[0]
				} else {
					e.Candidates = cands
					if !reflect.DeepEqual(prev.Candidates, cands) {
						log.Printf("remote keys: %s matches %d directories with different remotes; keeping its path key (set \"dir\" in %s to choose)",
							hash, len(cands), ProjectCachePath(m.syncDir))
					}
				}
			}
		}
		if dir != "" {
			e.Dir = dir
			key = m.keyOf(dir)
		}
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

// save writes the cache if resolution changed it.
func (m *RemoteMap) save() {
	if !m.dirty {
		return
	}
	m.dirty = false
	if err := m.cache.Save(m.syncDir); err != nil {
		log.Println("remote keys: save cache:", err)
	}
}

// splitRemotePath converts "remotes/<host>/<owner>/<repo>/memory/<file>" into
// its key ("<host>/<owner>/<repo>") and file name.
func splitRemotePath(p string) (key, name string, ok bool) {
	parts := strings.Split(filepath.ToSlash(p), "/")
	// remotes + at least host and repo + "memory" + file
	if len(parts) < 5 || parts[0] != remotesDir || parts[len(parts)-2] != "memory" {
		return "", "", false
	}
	return strings.Join(parts[1:len(parts)-2], "/"), parts[len(parts)-1], true
}
