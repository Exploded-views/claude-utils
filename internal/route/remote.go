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
