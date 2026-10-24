package route

import (
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
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

var windowsDrive = regexp.MustCompile(`^([A-Za-z])--(.+)$`)

// Candidates returns every existing directory whose escaped path equals the
// folder name. Usually one; the escaping is lossy, so it can be several
// (src/my-app and src/my.app) or none (deleted, or on another machine).
func Candidates(folder string) []string {
	folder = escape(folder)
	fold := func(s string) string { return s }
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		fold = strings.ToLower
	}
	if m := windowsDrive.FindStringSubmatch(folder); m != nil {
		if runtime.GOOS != "windows" {
			return nil
		}
		return walk(m[1]+`:\`, m[2], fold)
	}
	if strings.HasPrefix(folder, "-") && len(folder) > 1 {
		return walk(string(filepath.Separator), folder[1:], fold)
	}
	return nil
}

func walk(root, rest string, fold func(string) string) []string {
	var out []string
	var rec func(dir, rest string)
	rec = func(dir, rest string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			n := fold(escape(e.Name()))
			full := n == rest
			if !full && !(strings.HasPrefix(rest, n+"-") && len(rest) > len(n)+1) {
				continue
			}
			p := filepath.Join(dir, e.Name())
			if info, err := os.Stat(p); err != nil || !info.IsDir() {
				continue
			}
			if full {
				out = append(out, p)
			} else {
				rec(p, rest[len(n)+1:])
			}
		}
	}
	rec(root, fold(rest))
	sort.Strings(out)
	return out
}

// gitOrigin reports the origin URL of dir if dir is the top of a plain git
// checkout (a subdirectory or a linked worktree is deliberately not the repo).
// ok is false when git could not answer; true with url "" means "no origin".
func gitOrigin(dir string) (url string, ok bool) {
	git := func(args ...string) (string, error) {
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		c.Env = append(os.Environ(), "LC_ALL=C", "LANGUAGE=C")
		out, err := c.Output()
		return strings.TrimSpace(string(out)), err
	}
	top, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		var ee *exec.ExitError
		return "", errors.As(err, &ee) // git ran and said "not a repository"
	}
	ti, err1 := os.Stat(top)
	di, err2 := os.Stat(dir)
	if err1 != nil || err2 != nil || !os.SameFile(ti, di) {
		return "", err1 == nil && err2 == nil
	}
	gd, err1 := git("rev-parse", "--absolute-git-dir")
	common, err2 := git("rev-parse", "--path-format=absolute", "--git-common-dir")
	if err1 != nil || err2 != nil {
		return "", false
	}
	if filepath.Clean(gd) != filepath.Clean(common) {
		return "", true // linked worktree
	}
	u, err := git("config", "--local", "--get", "remote.origin.url")
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return "", true
		}
		return "", false
	}
	return u, true
}
