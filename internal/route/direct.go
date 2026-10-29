package route

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Route resolution asks three questions of every candidate directory: is it in
// a work tree, does its common dir resolve, what does HEAD name. Answering them
// by running git costs a process each, hundreds of times at start-up. The
// questions are answered here from the files git itself reads, and only when
// every file is as expected: on ANY doubt (a symlink, an odd file, a bare or
// foreign-owned repo, another filesystem, a file that cannot be read or parsed,
// a reftable repo, a symbolic ref chain) the answer is "ask git", never "no".

type directKind int

const (
	directDoubt directKind = iota // cannot tell from the files: ask git
	directNo                      // sure: not in a work tree / HEAD names no commit
	directYes                     // sure: in a work tree / HEAD is the commit returned
)

// repoLoc is where a checkout keeps its git data: gitDir holds HEAD, commonDir
// the objects, refs and config (the same directory unless a linked worktree).
type repoLoc struct {
	worktree, gitDir, commonDir string
}

var shaRe = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

func missing(err error) bool { return errors.Is(err, fs.ErrNotExist) }

// locateRepo finds the checkout containing dir the way git does: .git in dir
// or the nearest directory above it.
func locateRepo(dir string) (repoLoc, directKind) {
	if !filepath.IsAbs(dir) {
		return repoLoc{}, directDoubt
	}
	dir = filepath.Clean(dir)
	for _, part := range strings.Split(dir, string(filepath.Separator)) {
		if part == ".git" {
			return repoLoc{}, directDoubt // inside a git dir: git says "false"
		}
	}
	fi, err := os.Lstat(dir)
	switch {
	case missing(err):
		return repoLoc{}, directNo // git -C cannot enter it either
	case err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir():
		return repoLoc{}, directDoubt
	}
	me, haveMe := currentUID()
	uid, dev0, ok := fileIDs(fi)
	if !ok || !haveMe {
		return repoLoc{}, directDoubt
	}
	for d, curDev, curUID := dir, dev0, uid; ; {
		gitPath := filepath.Join(d, ".git")
		gfi, err := os.Lstat(gitPath)
		switch {
		case err == nil:
			if gfi.Mode()&os.ModeSymlink != 0 {
				return repoLoc{}, directDoubt
			}
			if curUID != me {
				return repoLoc{}, directDoubt // git's safe.directory check applies
			}
			loc, k := openGitPath(d, gitPath, gfi, me)
			return loc, k
		case !missing(err):
			return repoLoc{}, directDoubt
		}
		// d itself may be a bare repository (git stops there and says false).
		for _, n := range []string{"HEAD", "objects"} {
			if _, err := os.Lstat(filepath.Join(d, n)); err == nil || !missing(err) {
				return repoLoc{}, directDoubt
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			return repoLoc{}, directNo
		}
		pfi, err := os.Lstat(parent)
		if err != nil {
			return repoLoc{}, directDoubt
		}
		puid, pdev, ok := fileIDs(pfi)
		if !ok {
			return repoLoc{}, directDoubt
		}
		if pdev != curDev {
			return repoLoc{}, directDoubt // git stops at a filesystem boundary
		}
		d, curDev, curUID = parent, pdev, puid
	}
}

// openGitPath validates dir/.git, a directory or a "gitdir:" file.
func openGitPath(dir, gitPath string, fi os.FileInfo, me uint64) (repoLoc, directKind) {
	gitDir := gitPath
	if !fi.IsDir() {
		if !fi.Mode().IsRegular() || fi.Size() > 4096 {
			return repoLoc{}, directDoubt
		}
		b, err := os.ReadFile(gitPath)
		if err != nil {
			return repoLoc{}, directDoubt
		}
		line, ok := strings.CutPrefix(strings.TrimRight(string(b), "\r\n"), "gitdir: ")
		if !ok || line == "" || strings.ContainsAny(line, "\n\x00") {
			return repoLoc{}, directDoubt
		}
		if !filepath.IsAbs(line) {
			line = filepath.Join(dir, line)
		}
		gitDir = filepath.Clean(line)
	}
	gi, err := os.Lstat(gitDir)
	if err != nil || gi.Mode()&os.ModeSymlink != 0 || !gi.IsDir() {
		return repoLoc{}, directDoubt
	}
	if uid, _, ok := fileIDs(gi); !ok || uid != me {
		return repoLoc{}, directDoubt
	}
	if !validHEADFile(gitDir) {
		return repoLoc{}, directDoubt
	}
	common := gitDir
	if b, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		c := strings.TrimRight(string(b), "\r\n")
		if c == "" || strings.ContainsAny(c, "\n\x00") {
			return repoLoc{}, directDoubt
		}
		if !filepath.IsAbs(c) {
			c = filepath.Join(gitDir, c)
		}
		common = filepath.Clean(c)
	} else if !missing(err) {
		return repoLoc{}, directDoubt
	}
	for _, sub := range []string{"objects", "refs"} {
		si, err := os.Lstat(filepath.Join(common, sub))
		if err != nil || si.Mode()&os.ModeSymlink != 0 || !si.IsDir() {
			return repoLoc{}, directDoubt
		}
	}
	if _, err := os.Lstat(filepath.Join(gitDir, "config.worktree")); err == nil || !missing(err) {
		return repoLoc{}, directDoubt
	}
	bare, wt, ok := readCoreConfig(filepath.Join(common, "config"))
	if !ok || bare {
		return repoLoc{}, directDoubt
	}
	if wt != "" {
		if !filepath.IsAbs(wt) {
			wt = filepath.Join(common, wt)
		}
		a, e1 := filepath.EvalSymlinks(wt)
		b, e2 := filepath.EvalSymlinks(dir)
		if e1 != nil || e2 != nil || a != b {
			return repoLoc{}, directDoubt
		}
	}
	return repoLoc{worktree: dir, gitDir: gitDir, commonDir: common}, directYes
}

// validHEADFile is git's validate_headref: HEAD is a "ref: refs/..." line or a
// full object name.
func validHEADFile(gitDir string) bool {
	b, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil || len(b) > 4096 {
		return false
	}
	h := strings.TrimSpace(string(b))
	if ref, ok := strings.CutPrefix(h, "ref: "); ok {
		return strings.HasPrefix(strings.TrimSpace(ref), "refs/")
	}
	return shaRe.MatchString(h)
}

// readCoreConfig reads core.bare and core.worktree from a config file without
// following includes. ok is false when the file cannot be read, or holds an
// include (which could set either).
func readCoreConfig(path string) (bare bool, worktree string, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil || len(b) > 1<<20 {
		return false, "", false
	}
	section := ""
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || l[0] == '#' || l[0] == ';' {
			continue
		}
		if l[0] == '[' {
			end := strings.IndexByte(l, ']')
			if end < 0 {
				return false, "", false
			}
			f := strings.Fields(strings.Trim(l[:end], "[]"))
			if len(f) == 0 {
				return false, "", false
			}
			section = strings.ToLower(f[0])
			if strings.HasPrefix(section, "include") {
				return false, "", false
			}
			if rest := strings.TrimSpace(l[end+1:]); rest != "" && !strings.HasPrefix(rest, "#") && !strings.HasPrefix(rest, ";") {
				return false, "", false // a key on the section line: too odd to parse
			}
			continue
		}
		if section != "core" {
			continue
		}
		k, v, _ := strings.Cut(l, "=")
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		switch k {
		case "bare":
			if v != "false" && v != "no" && v != "off" && v != "0" {
				bare = true
			}
		case "worktree":
			if strings.ContainsAny(v, "\"\\") {
				return false, "", false
			}
			worktree = v
		}
	}
	return bare, worktree, true
}

// directInside answers `git rev-parse --is-inside-work-tree` plus
// `--git-common-dir` for dir: Yes when both would succeed with "true", No when
// git would fail or say "false".
func directInside(dir string) directKind {
	_, k := locateRepo(dir)
	return k
}

// directHEAD answers `git rev-parse HEAD` for the checkout containing dir:
// Yes with the commit, No when HEAD names no commit yet (unborn branch).
func directHEAD(dir string) (string, directKind) {
	loc, k := locateRepo(dir)
	if k != directYes {
		return "", directDoubt // not a repo, or odd: git gives the error text and decides
	}
	if _, err := os.Lstat(filepath.Join(loc.commonDir, "reftable")); err == nil || !missing(err) {
		return "", directDoubt
	}
	if cb, err := os.ReadFile(filepath.Join(loc.commonDir, "config")); err != nil || strings.Contains(strings.ToLower(string(cb)), "refstorage") {
		return "", directDoubt
	}
	hb, err := os.ReadFile(filepath.Join(loc.gitDir, "HEAD"))
	if err != nil {
		return "", directDoubt
	}
	h := strings.TrimSpace(string(hb))
	ref, symbolic := strings.CutPrefix(h, "ref: ")
	if !symbolic {
		if shaRe.MatchString(h) {
			return h, directYes
		}
		return "", directDoubt
	}
	ref = strings.TrimSpace(ref)
	if !safeRef(ref) {
		return "", directDoubt
	}
	base := loc.commonDir
	for _, p := range []string{"refs/bisect/", "refs/worktree/", "refs/rewritten/"} {
		if strings.HasPrefix(ref, p) {
			base = loc.gitDir
		}
	}
	rb, err := os.ReadFile(filepath.Join(base, filepath.FromSlash(ref)))
	if err == nil {
		if v := strings.TrimSpace(string(rb)); shaRe.MatchString(v) {
			return v, directYes
		}
		return "", directDoubt // a symbolic ref, or not a ref file
	}
	if !missing(err) {
		return "", directDoubt
	}
	pb, err := os.ReadFile(filepath.Join(loc.commonDir, "packed-refs"))
	if err != nil && !missing(err) {
		return "", directDoubt
	}
	for _, l := range strings.Split(string(pb), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || l[0] == '#' || l[0] == '^' {
			continue
		}
		if v, name, ok := strings.Cut(l, " "); ok && name == ref {
			if shaRe.MatchString(v) {
				return v, directYes
			}
			return "", directDoubt
		}
	}
	return "", directNo // unborn: git fails with "ambiguous argument HEAD"
}

func safeRef(ref string) bool {
	if !strings.HasPrefix(ref, "refs/") || strings.Contains(ref, "..") || strings.Contains(ref, "//") ||
		strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".lock") || strings.ContainsAny(ref, "\\\x00") {
		return false
	}
	for _, c := range ref {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}
