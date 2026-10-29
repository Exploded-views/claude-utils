package route

import (
	"context"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Remote resolution, ported from the remote-key work: the escaped folder name
// Claude uses is turned back into the existing directory it names, whose git
// origin is normalised to host/owner/repo.

// escape mirrors how Claude Code names a project folder: every character that
// is not an ASCII letter or digit becomes '-'.
func escape(p string) string {
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

// NormalizeRemote turns a git remote URL into host/owner/repo (more segments
// for nested groups). ssh and https, with or without ".git", credentials, a
// port, or different case give the same key. It reports false for anything
// that is not a stable identity across machines: local paths, file:// URLs,
// URLs without an owner and a repo.
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
		return "", false // Windows drive path
	default:
		m := scpLike.FindStringSubmatch(raw)
		if m == nil {
			return "", false
		}
		host, path = m[1], m[2]
	}
	if strings.Contains(host, "@") {
		return "", false
	}
	host = sanitizeSegment(strings.ToLower(host))
	if host == "ssh.github.com" {
		host = "github.com" // GitHub's ssh-over-443 endpoint, as ghguard treats it
	}
	if host == "" {
		return "", false
	}
	path = strings.TrimSuffix(strings.Trim(strings.ToLower(path), "/"), ".git")
	segs := strings.Split(path, "/")
	if len(segs) < 2 {
		return "", false
	}
	for i, s := range segs {
		if s == "" || s == "." || s == ".." || strings.Contains(s, "@") {
			return "", false
		}
		if segs[i] = sanitizeSegment(s); segs[i] == "" {
			return "", false
		}
	}
	return host + "/" + strings.Join(segs, "/"), true
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

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
	return strings.TrimRight(b.String(), ".")
}

// readDir and walkHook are test seams: the directory reader, and a hook called
// with the folder each time a walk starts.
var (
	readDir  = os.ReadDir
	walkHook func(folder string)
)

var windowsDrive = regexp.MustCompile(`^([A-Za-z])--(.+)$`)

// Candidates returns every existing directory whose escaped path equals the
// folder name. Usually one; the escaping is lossy, so it can be several
// (src/my-app and src/my.app) or none (deleted, or on another machine).
func Candidates(folder string) []string {
	return candidates(folder, func(dir string) *listing { return readListing(dir) })
}

// platformFold is the case fold used to compare path components.
func platformFold() func(string) string {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.ToLower
	}
	return func(s string) string { return s }
}

// listing is one directory's entries indexed by escaped, case-folded name,
// with each child's is-a-directory answer filled in on first use.
type listing struct {
	dir   string
	byKey map[string][]string
	mu    sync.Mutex
	isDir map[string]bool
}

// readListing reads dir; an unreadable one lists as empty.
func readListing(dir string) *listing {
	l := &listing{dir: dir, byKey: map[string][]string{}, isDir: map[string]bool{}}
	entries, err := readDir(dir)
	if err != nil {
		return l
	}
	fold := platformFold()
	for _, e := range entries {
		k := fold(escape(e.Name()))
		l.byKey[k] = append(l.byKey[k], e.Name())
	}
	return l
}

// subdir reports whether dir/name is a directory (following symlinks). The
// answer lives and dies with this listing.
func (l *listing) subdir(name string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	d, ok := l.isDir[name]
	if !ok {
		info, err := os.Stat(filepath.Join(l.dir, name))
		d = err == nil && info.IsDir()
		l.isDir[name] = d
	}
	return d
}

func candidates(folder string, list func(dir string) *listing) []string {
	if walkHook != nil {
		walkHook(folder)
	}
	folder = escape(folder)
	fold := platformFold()
	if m := windowsDrive.FindStringSubmatch(folder); m != nil {
		if runtime.GOOS != "windows" {
			return nil
		}
		return walk(m[1]+`:\`, m[2], fold, list)
	}
	if strings.HasPrefix(folder, "-") && len(folder) > 1 {
		return walk(string(filepath.Separator), folder[1:], fold, list)
	}
	return nil
}

func walk(root, rest string, fold func(string) string, list func(string) *listing) []string {
	var out []string
	var rec func(dir, rest string)
	rec = func(dir, rest string) {
		l := list(dir)
		if l == nil {
			return
		}
		try := func(n string, full bool) {
			for _, name := range l.byKey[n] {
				if !l.subdir(name) {
					continue
				}
				p := filepath.Join(dir, name)
				if full {
					out = append(out, p)
				} else {
					rec(p, rest[len(n)+1:])
				}
			}
		}
		try(rest, true)
		// A shorter entry name followed by '-' and something more.
		for i := 1; i < len(rest)-1; i++ {
			if rest[i] == '-' {
				try(rest[:i], false)
			}
		}
	}
	rec(root, fold(rest))
	sort.Strings(out)
	return out
}

// stamp is what tells a directory changed: a new, removed or renamed entry
// moves its mtime (and usually its size).
type stamp struct {
	mod, size int64
	ok        bool
}

func stampOf(dir string) stamp {
	info, err := os.Stat(dir)
	if err != nil {
		return stamp{}
	}
	return stamp{info.ModTime().UnixNano(), info.Size(), true}
}

// walkCache holds one generation's directory listings and Candidates answers.
// A listing is used only while its directory's stamp is unchanged, so a folder
// created after the first look is seen by the next decision; each listing is
// read once however many callers ask at the same time. The Router swaps in a
// fresh cache on Replace and Refresh.
type walkCache struct {
	mu    sync.Mutex
	dirs  map[string]*stamped
	cands map[string]*candEntry
}

type stamped struct {
	st   stamp
	once sync.Once
	l    *listing
}

type dep struct {
	dir string
	st  stamp
}

// candEntry is a Candidates answer and the stamps of the directories it read.
type candEntry struct {
	once sync.Once
	deps []dep
	v    []string
}

func newWalkCache() *walkCache {
	return &walkCache{dirs: map[string]*stamped{}, cands: map[string]*candEntry{}}
}

// list gives the listing of dir for its current stamp, with that stamp; nil
// when dir cannot be stat'ed.
func (c *walkCache) list(dir string) (*listing, stamp) {
	st := stampOf(dir)
	if !st.ok {
		return nil, st
	}
	c.mu.Lock()
	e := c.dirs[dir]
	if e == nil || e.st != st {
		e = &stamped{st: st}
		c.dirs[dir] = e
	}
	c.mu.Unlock()
	e.once.Do(func() { e.l = readListing(dir) })
	return e.l, st
}

// maxWalks bounds the walks of one Candidates call: a directory that changes
// all the time (a busy temp dir) must not keep it walking.
const maxWalks = 3

func (c *walkCache) candidates(folder string) []string {
	for try := 1; ; try++ {
		c.mu.Lock()
		e := c.cands[folder]
		if e == nil {
			e = &candEntry{}
			c.cands[folder] = e
		}
		c.mu.Unlock()
		e.once.Do(func() {
			e.v = candidates(folder, func(dir string) *listing {
				l, st := c.list(dir)
				e.deps = append(e.deps, dep{dir, st})
				return l
			})
		})
		fresh := true
		for _, d := range e.deps {
			if stampOf(d.dir) != d.st {
				fresh = false
				break
			}
		}
		if fresh {
			return e.v
		}
		// A directory it read has changed: drop this answer and walk again,
		// or after the last walk hand it back without remembering it.
		c.mu.Lock()
		if c.cands[folder] == e {
			delete(c.cands, folder)
		}
		c.mu.Unlock()
		if try == maxWalks {
			return e.v
		}
	}
}

// remoteURL is the origin URL of the checkout that contains dir, with the
// user's url.<base>.insteadOf rewrites applied (git remote get-url). dir may be
// the top of a checkout, a subdirectory of one, or a linked worktree: all of
// them read the one config of the containing repository. ok is false when dir
// is not in a work tree or git could not answer; url "" means no origin.
func remoteURL(dir string) (url string, ok bool) {
	git := func(args ...string) (string, error) { return gitOut(dir, args...) }
	if in, err := git("rev-parse", "--is-inside-work-tree"); err != nil || in != "true" {
		return "", false
	}
	// The shared config holds the remote; this also proves a worktree's
	// common dir resolves.
	if _, err := git("rev-parse", "--git-common-dir"); err != nil {
		return "", false
	}
	u, err := git("remote", "get-url", "origin")
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", true // no such remote
		}
		return "", false
	}
	return u, true
}

// gitOut runs git in dir with the inherited GIT_* variables dropped.
func gitOut(dir string, args ...string) (string, error) {
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	c.Env = append(cleanGitEnv(os.Environ()), "LC_ALL=C", "LANGUAGE=C")
	out, err := c.Output()
	return strings.TrimSpace(string(out)), err
}

// sshHostname asks ssh which real host an alias stands for (ssh -G reads the
// user's ssh config and does not connect). "" when it cannot say.
func sshHostname(host string) string {
	if host == "" || strings.HasPrefix(host, "-") {
		return "" // would be read as an ssh option
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ssh", "-G", "--", host).Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) == 2 && strings.EqualFold(f[0], "hostname") {
			return f[1]
		}
	}
	return ""
}

// rewriteSSHHost replaces the host of an ssh or scp-style URL by what lookup
// says it really is, so an alias from ssh config gives the key of the real host.
func rewriteSSHHost(raw string, lookup func(string) string) string {
	if lookup == nil {
		return raw
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "ssh" && u.Scheme != "git+ssh" && u.Scheme != "ssh+git") {
			return raw
		}
		if real := lookup(u.Hostname()); real != "" && real != u.Hostname() {
			if p := u.Port(); p != "" {
				u.Host = real + ":" + p
			} else {
				u.Host = real
			}
			return u.String()
		}
		return raw
	}
	if m := scpLike.FindStringSubmatchIndex(raw); m != nil && !(len(raw) > 1 && raw[1] == ':' && isLetter(raw[0])) {
		host := raw[m[2]:m[3]]
		if real := lookup(host); real != "" && real != host {
			return raw[:m[2]] + real + raw[m[3]:]
		}
	}
	return raw
}

// cleanGitEnv drops the inherited GIT_* variables (GIT_DIR, GIT_WORK_TREE,
// GIT_CONFIG_*, ...): they can point git at another repository or config than
// the project directory's own, and so at another remote.
func cleanGitEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(strings.ToUpper(kv), "GIT_") && !strings.HasPrefix(strings.ToUpper(kv), "GIT_EXEC_PATH=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}
