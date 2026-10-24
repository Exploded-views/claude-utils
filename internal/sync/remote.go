package sync

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
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
		// '@' left in a path is the tail of a URL password that contained
		// an unescaped '/'; the real host is not what we parsed.
		if s == "" || s == "." || s == ".." || strings.Contains(s, "@") {
			return "", false
		}
		segs[i] = sanitizeSegment(s)
	}
	return host + "/" + strings.Join(segs, "/"), true
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// windowsReserved are device names Windows refuses as a file name, with or
// without an extension.
var windowsReserved = regexp.MustCompile(`^(con|prn|aux|nul|com[1-9]|lpt[1-9])(\.|$)`)

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
	out := b.String()
	if windowsReserved.MatchString(out) {
		out = "_" + out
	}
	return out
}

// validKey reports whether k is acceptable as a repo-side key: what
// NormalizeRemote can produce. At least two segments, each of [a-z0-9._-]
// and neither "." nor "..", so a key can never leave remotes/ or collide with
// a one-segment path key.
func validKey(k string) bool {
	segs := strings.Split(k, "/")
	if len(segs) < 2 {
		return false
	}
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return false
		}
		for _, r := range s {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_') {
				return false
			}
		}
	}
	return true
}

var windowsDrive = regexp.MustCompile(`^([A-Za-z])--(.+)$`)

// candidateRoots says where Candidates starts walking and how names compare.
// The zero value resolves nothing; defaultCandidateRoots is the real thing.
type candidateRoots struct {
	Unix  string                     // directory a "-home-..." name is resolved under
	Drive func(letter string) string // root of a "C--..." name's drive
	Fold  func(string) string        // name normalisation for matching
}

func defaultCandidateRoots() candidateRoots {
	return candidateRoots{
		Unix:  string(filepath.Separator),
		Drive: func(letter string) string { return letter + `:\` },
		Fold:  foldFor(runtime.GOOS),
	}
}

// foldFor returns the name normalisation to use on goos: lower-casing where the
// file system is case-insensitive by default (Windows, macOS), none elsewhere.
func foldFor(goos string) func(string) string {
	if goos == "windows" || goos == "darwin" {
		return strings.ToLower
	}
	return func(s string) string { return s }
}

// Candidates returns every existing directory whose escaped path equals the
// given project folder name. Usually that is exactly one. The escaping is
// lossy, so it can be several (src/my-app, src/my.app and src/my/app all
// escape alike) or none (the project was deleted or lives on another
// machine). Results are sorted.
func Candidates(escaped string) []string {
	return candidatesIn(escaped, defaultCandidateRoots())
}

func candidatesIn(escaped string, env candidateRoots) []string {
	escaped = EscapeProjectPath(escaped) // tolerate '_' etc. kept by other versions
	if m := windowsDrive.FindStringSubmatch(escaped); m != nil {
		if env.Drive == nil {
			return nil
		}
		return walkCandidatesFold(env.Drive(m[1]), m[2], env.Fold)
	}
	if strings.HasPrefix(escaped, "-") && len(escaped) > 1 && env.Unix != "" {
		return walkCandidatesFold(env.Unix, escaped[1:], env.Fold)
	}
	return nil
}

// walkCandidates finds the directories below root whose path, escaped,
// equals rest, comparing names exactly as the host OS would.
func walkCandidates(root, rest string) []string {
	return walkCandidatesFold(root, rest, defaultCandidateRoots().Fold)
}

// walkCandidatesFold is walkCandidates with names compared after fold. It only
// descends into entries that can still match, so the cost is proportional to
// the siblings along the one real path.
func walkCandidatesFold(root, rest string, fold func(string) string) []string {
	if fold == nil {
		fold = func(s string) string { return s }
	}
	var out []string
	var walk func(dir, rest string)
	walk = func(dir, rest string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			n := fold(EscapeProjectPath(e.Name()))
			full := n == rest
			if !full && !(strings.HasPrefix(rest, n+"-") && len(rest) > len(n)+1) {
				continue // cheap name test first; only then stat a symlink
			}
			if !isDirEntry(dir, e) {
				continue
			}
			if full {
				out = append(out, filepath.Join(dir, e.Name()))
			} else {
				walk(filepath.Join(dir, e.Name()), rest[len(n)+1:])
			}
		}
	}
	walk(root, fold(rest))
	sort.Strings(out)
	return out
}

// isDir reports whether p is an existing directory.
func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
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

// gitOrigin reports the origin URL of dir if dir is the top of a plain git
// checkout. A subdirectory of a repo is deliberately not treated as the repo
// (two subprojects would otherwise share one memory folder), and neither is a
// linked worktree: current Claude Code keeps one auto-memory folder per
// repository, shared by all its worktrees, so a worktree's own project folder
// only holds transcripts.
//
// ok is false when the answer could not be determined (directory missing, git
// not installed, dubious ownership, any other git failure). Only when git ran
// and said "no origin", "not a repository" or the like is it true with url "".
func gitOrigin(dir string) (url string, ok bool) {
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", false
	}
	r := gitwt.New(dir)
	out, err := r.Run("rev-parse", "--show-toplevel", "--git-dir", "--git-common-dir")
	if err != nil {
		return "", strings.Contains(err.Error(), "not a git repository")
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		return "", false
	}
	ti, err1 := os.Stat(strings.TrimSpace(lines[0]))
	di, err2 := os.Stat(dir)
	if err1 != nil || err2 != nil {
		return "", false
	}
	if !os.SameFile(ti, di) {
		return "", true
	}
	gitDir, ok1 := resolveGitPath(dir, lines[1])
	common, ok2 := resolveGitPath(dir, lines[2])
	if !ok1 || !ok2 {
		return "", false
	}
	if gitDir != common {
		return "", true // linked worktree
	}
	cfg, err := r.Run("config", "--local", "--get", "remote.origin.url")
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return "", true // git config's "no such key"
		}
		return "", false
	}
	return strings.TrimSpace(cfg), true
}

// resolveGitPath makes a path printed by git rev-parse (relative to dir
// unless absolute) comparable.
func resolveGitPath(dir, p string) (string, bool) {
	p = strings.TrimSpace(p)
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	p, err := filepath.EvalSymlinks(p)
	return p, err == nil
}

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
	// Lookup returns the origin URL for a project directory, "" if it has
	// none, and false if git could not tell. Replaceable in tests.
	Lookup func(dir string) (url string, ok bool)

	cache  *ProjectCache
	keys   map[string]string // folder name -> key; "" means path key
	fresh  map[string]bool   // folders whose key was positively resolved this run, not just remembered
	warned map[string]bool   // invalid overrides already logged, by folder name
	dirty  bool              // cache changed since it was last saved
}

// NewRemoteMap loads the per-PC cache and resolves every project folder.
func NewRemoteMap(syncDir, claudeRoot string) (*RemoteMap, error) {
	c, err := LoadProjectCache(syncDir)
	if err != nil {
		return nil, err
	}
	m := &RemoteMap{syncDir: syncDir, claudeRoot: claudeRoot, Lookup: gitOrigin, cache: c, keys: map[string]string{}, fresh: map[string]bool{}, warned: map[string]bool{}}
	m.Refresh()
	return m, nil
}

// Refresh re-resolves every project folder under the Claude root and saves
// the cache if anything changed.
func (m *RemoteMap) Refresh() {
	m.keys = map[string]string{}
	m.fresh = map[string]bool{}
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
	if !isDir(filepath.Join(m.claudeRoot, hash)) {
		return "" // not a project folder on this PC (e.g. a hash from another PC's push)
	}
	k := m.resolve(hash)
	m.save()
	return k
}

// Member reports whether the project folder takes part in its group: it is
// filled from the shared notes and its own changes are shared. That holds for a
// folder whose key was positively resolved on this run (git reported the origin,
// or the cache pins a key), even before it has a memory/ dir: gitOrigin keys
// linked worktrees and subdirectories by path, so a remote-keyed folder is
// always a repo's own folder and a fresh clone should get the shared notes. A
// folder that only remembers its key (the checkout is gone, git will not
// answer) takes part for as long as it has a memory/ dir, but is never filled.
func (m *RemoteMap) Member(hash string) bool {
	if m.keys[hash] == "" {
		return false
	}
	return m.fresh[hash] || isDir(filepath.Join(m.claudeRoot, hash, "memory"))
}

// Members lists the known project folders stored under key that take part in
// the group (see Member), sorted.
func (m *RemoteMap) Members(key string) []string {
	var out []string
	for h, k := range m.keys {
		if k == key && m.Member(h) {
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

func (m *RemoteMap) resolve(hash string) string {
	prev, had := m.cache.Projects[hash]
	e := prev
	var key string

	override := e.Key // an invalid one is ignored but left in the file for the user to fix
	if override != "" && override != "path" && !validKey(override) {
		if !m.warned[hash] {
			m.warned[hash] = true
			log.Printf("remote keys: ignoring invalid \"key\" %q for %s in %s (want host/owner/repo of [a-z0-9._-])",
				override, hash, ProjectCachePath(m.syncDir))
		}
		override = ""
	}

	switch {
	case override == "path":
	case override != "":
		key = override
		m.fresh[hash] = true
	default:
		// Keys are sticky: a deleted checkout, an unmounted drive or a git
		// that will not answer must not flip a folder back to its path key
		// and fork its notes. Only git saying "no origin" or a different
		// origin changes the key.
		key = lastKey(e)
		dir := e.Dir
		if dir != "" {
			if info, err := os.Stat(dir); err != nil || !info.IsDir() {
				dir = "" // moved or deleted since last time
			}
		}
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
					e.Dir = ""
					e.Candidates = cands
					if !reflect.DeepEqual(prev.Candidates, cands) {
						log.Printf("remote keys: %s matches %d directories with different remotes; keeping its previous key (set \"dir\" in %s to choose)",
							hash, len(cands), ProjectCachePath(m.syncDir))
					}
				}
			}
		}
		if dir != "" {
			e.Dir = dir
			if k, ok := m.keyOf(dir); ok {
				key = k
				e.LastKey = k
				m.fresh[hash] = k != ""
			}
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
