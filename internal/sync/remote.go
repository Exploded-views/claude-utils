package sync

import (
	"errors"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

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
// remote key is at least three) and so that older daemons, which only look
// inside `projects/`, simply ignore it. Projects with no usable remote keep
// their path key under `projects/`.
//
// A remote key has at most one local folder per PC (see RemoteMap). That folder
// is synced exactly like a path-keyed one; only its repo-side directory differs.

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
	if strings.Contains(host, "@") {
		// The tail of a password that itself contained '@' (scp syntax:
		// user:p@ss@host:o/r); the real host is not what we parsed.
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
		// '@' left in a path is the tail of a URL password that contained
		// an unescaped '/'; the real host is not what we parsed.
		if s == "" || s == "." || s == ".." || strings.Contains(s, "@") {
			return "", false
		}
		if segs[i] = sanitizeSegment(s); segs[i] == "" {
			return "", false // nothing but dots
		}
	}
	return host + "/" + strings.Join(segs, "/"), true
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// windowsReserved are device names Windows refuses as a file name, with or
// without an extension.
var windowsReserved = regexp.MustCompile(`^(con|prn|aux|nul|com[1-9]|lpt[1-9])(\.|$)`)

// sanitizeSegment keeps a path segment safe to use as a directory name on
// every platform: Windows silently drops trailing dots, so they are removed
// here rather than have two spellings share a directory on some PCs only.
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
	out := strings.TrimRight(b.String(), ".")
	if windowsReserved.MatchString(out) {
		out = "_" + out
	}
	return out
}

// validKey reports whether k is acceptable as a repo-side key: what
// NormalizeRemote can produce. At least three segments (host, owner, repo),
// each of [a-z0-9._-] and neither "." nor "..", so a key can never leave
// remotes/ or collide with a one-segment path key. A segment must also survive sanitizeSegment
// unchanged, which rules out a trailing dot and the Windows device names.
func validKey(k string) bool {
	segs := strings.Split(k, "/")
	if len(segs) < 3 {
		return false
	}
	for _, s := range segs {
		if s == "" || s == "." || s == ".." || strings.HasSuffix(s, ".") || sanitizeSegment(s) != s {
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

// isDirEntry reports whether e is a directory, following symlinks. On Windows
// a junction is reported as irregular rather than as a symlink, so that type is
// stat-ed too.
func isDirEntry(parent string, e fs.DirEntry) bool {
	if e.IsDir() {
		return true
	}
	if e.Type()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
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
	r := gitRepo(dir)
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

// gitRepo is the repo to run read-only questions about a project directory in.
// Callers match git's English messages ("not a git repository", "dubious
// ownership"), so git must not translate them to the user's language.
func gitRepo(dir string) *gitwt.Repo {
	r := gitwt.New(dir)
	r.Env = []string{"LC_ALL=C", "LANGUAGE=C"}
	return r
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

// gitFailure says why git could not answer for dir, as a class that is safe to
// log (see classifyGitFailure).
func gitFailure(dir string) string {
	_, err := gitRepo(dir).Run("rev-parse", "--git-dir")
	if err == nil {
		return "other"
	}
	return classifyGitFailure(err)
}

// classifyGitFailure sorts a failed git run into "dubious ownership", "git not
// found" or "other". Git runs in the C locale (see gitRepo), so its messages
// can be matched.
func classifyGitFailure(err error) string {
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return "git not found"
	case strings.Contains(err.Error(), "dubious ownership"):
		return "dubious ownership"
	}
	return "other"
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

// claudeFolderFor maps a path in the sync repo, as `git diff` prints it, to the
// local Claude project folder and file name it belongs to: the folder named by
// projects/<hash>/memory/<file>, or the one folder holding the key of
// remotes/<key>/memory/<file>. ok is false for a path that is not for this PC's
// Claude tree: not a memory file, a remote nobody here holds, or the old
// projects/<hash> copy of a folder that is remote-keyed now, or a conflict
// backup (see isBackupName) that an older client pushed.
func claudeFolderFor(roots Roots, repoPath string) (hash, name string, ok bool) {
	if isBackupName(filepath.Base(filepath.FromSlash(repoPath))) {
		return "", "", false // a local-only backup that reached the repo anyway
	}
	if hash, name, ok := splitMirrorPath(repoPath); ok {
		if roots.Remote != nil && roots.Remote.Key(hash) != "" {
			return "", "", false // superseded by its remote-keyed copy
		}
		return hash, name, true
	}
	if roots.Remote == nil {
		return "", "", false
	}
	key, name, ok := splitRemotePath(repoPath)
	if !ok {
		return "", "", false
	}
	hash, ok = roots.Remote.FolderFor(key)
	if !ok || !isDir(filepath.Join(roots.Claude, hash)) {
		return "", "", false
	}
	return hash, name, true
}
