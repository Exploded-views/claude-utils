package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Three pieces say which build the user service runs: the unit's
// ConditionFileIsExecutable= (written by kardianos at the first install and by
// nothing else), its ExecStart=, and the bin/ symlinks that bootstrap repoints
// at lib/<id>/. An upgrade that stops half way, or skips a piece because it
// "looks current", leaves them on different builds. install rewrites all of
// them together; doctor reports any disagreement.

// unitVersionID is the stable id of the doctor check that the pieces naming
// the running build agree.
const unitVersionID = "unit-version"

const conditionKey = "ConditionFileIsExecutable="

// binLinkNames are the bin/ symlinks bootstrap makes into lib/<id>/.
var binLinkNames = []string{"claude-memsync", "claude-memmerge"}

// unitPiece is one place that says which build runs.
type unitPiece struct {
	Name string // "Condition", "ExecStart" or "symlink"
	Path string // what it points at, as written
}

// checkResult is the outcome of one doctor check.
type checkResult struct {
	ID     string
	OK     bool
	Detail string
	Pieces []unitPiece
}

// binLinkDirFn is the directory holding the bin/ symlinks; tests replace it.
var binLinkDirFn = func() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "bin"), nil
}

// installExeFn is the executable being installed; tests replace it.
var installExeFn = os.Executable

// exeThrowawayFn reports a binary that must not become the installed build;
// tests replace it.
var exeThrowawayFn = exeUnsafe

// fileOwnerFn is the owner uid and group gid of a file; tests replace it to
// fake owners. ok is false where the platform has none.
var fileOwnerFn = platformOwner

// tempDirFn is the temp dir; ancestorsFrom, when set, makes the ancestor walk
// ignore that directory and everything above it. Tests replace both.
var (
	tempDirFn     = os.TempDir
	ancestorsFrom = ""
)

// permIssue refuses group- and world-write. A user-private group can still
// have supplementary members, so group-write is never excused. The group case
// says how to fix it; messages carry paths only.
func permIssue(st fs.FileInfo, p string) error {
	perm := st.Mode().Perm()
	if perm&0o002 != 0 {
		return fmt.Errorf("%s is world-writable; run: chmod o-w %s (or re-run bootstrap)", p, p)
	}
	if perm&0o020 != 0 {
		return fmt.Errorf("%s is group-writable; run: chmod g-w %s (or re-run bootstrap)", p, p)
	}
	return nil
}

// exeUnsafe tells why a binary must not become the installed build, or nil.
// The path is made absolute and symlinks are resolved first. Refused: an
// unresolvable exe, one under the resolved temp dir, a file that is not a
// regular file, is group- or world-writable or is owned by neither the user nor root, and any
// ancestor directory with the same faults. Root-owned directories pass.
func exeUnsafe(exe string) error {
	abs, err := filepath.Abs(exe)
	if err != nil {
		return fmt.Errorf("%s cannot be resolved: %w", exe, err)
	}
	r, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return fmt.Errorf("%s cannot be resolved: %w", exe, err)
	}
	if t, err := filepath.EvalSymlinks(tempDirFn()); err == nil && underDir(r, t) {
		return fmt.Errorf("%s runs from a temp dir", r)
	}
	st, err := os.Lstat(r)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", r)
	}
	if err := permIssue(st, r); err != nil {
		return err
	}
	if uid, _, ok := fileOwnerFn(st); !ok || (uid != currentUID() && uid != 0) {
		return errors.New("the executable is not owned by you or root")
	}
	stop := ""
	if ancestorsFrom != "" {
		stop, _ = filepath.EvalSymlinks(ancestorsFrom)
	}
	for d := filepath.Dir(r); ; d = filepath.Dir(d) {
		if stop != "" && (d == stop || !strings.HasPrefix(d, stop+string(filepath.Separator))) {
			break
		}
		ds, err := os.Stat(d)
		if err != nil {
			return err
		}
		if err := permIssue(ds, d); err != nil {
			return err
		}
		if uid, _, ok := fileOwnerFn(ds); !ok || (uid != currentUID() && uid != 0) {
			return fmt.Errorf("%s is owned by uid %d", d, uid)
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	return nil
}

// libRootFn is the bootstrap lib root, ~/.local/lib/claude-memsync; tests
// replace it.
var libRootFn = func() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "lib", "claude-memsync")
}

// currentUID is the user whose files may be linked; tests replace it.
var currentUID = os.Getuid

// sectionLine is the index of the first line with the prefix inside section.
func sectionLine(lines []string, section, prefix string) int {
	in := false
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			in = t == section
			continue
		}
		if in && strings.HasPrefix(strings.TrimSuffix(l, "\r"), prefix) {
			return i
		}
	}
	return -1
}

func unescapeUnitPath(s string) string { return strings.ReplaceAll(s, `\x20`, " ") }

// currentCondition is the [Unit] ConditionFileIsExecutable path.
func currentCondition(content string) (string, bool) {
	lines := strings.Split(content, "\n")
	if i := sectionLine(lines, "[Unit]", conditionKey); i >= 0 {
		return unescapeUnitPath(strings.TrimPrefix(strings.TrimSuffix(lines[i], "\r"), conditionKey)), true
	}
	return "", false
}

// setCondition points the [Unit] ConditionFileIsExecutable line at exe. A
// unit without the line, or with it already right, is no change.
func setCondition(content, exe string) (string, bool) {
	lines := strings.Split(content, "\n")
	i := sectionLine(lines, "[Unit]", conditionKey)
	if i < 0 {
		return content, false
	}
	want := conditionKey + strings.ReplaceAll(exe, " ", `\x20`)
	cur := strings.TrimSuffix(lines[i], "\r")
	if cur == want {
		return content, false
	}
	lines[i] = want + lines[i][len(cur):]
	return strings.Join(lines, "\n"), true
}

func sameFile(a, b string) bool {
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		ra = filepath.Clean(a)
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		rb = filepath.Clean(b)
	}
	return ra == rb
}

// underDir reports p (symlinks resolved) inside dir (likewise).
func underDir(p, dir string) bool {
	if dir == "" {
		return false
	}
	rp, err := filepath.EvalSymlinks(p)
	if err != nil {
		return false
	}
	rd, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false
	}
	return strings.HasPrefix(rp, rd+string(filepath.Separator))
}

// linkTargetSafe says whether p may become a bin/ symlink target: a regular
// file (not a link), owned by the current user, not group- or world-writable.
func linkTargetSafe(p string) error {
	st, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", p)
	}
	if err := permIssue(st, p); err != nil {
		return err
	}
	if uid, _, ok := fileOwnerFn(st); !ok || uid != currentUID() {
		return fmt.Errorf("%s is not owned by the current user", p)
	}
	return nil
}

// relinkBins repoints each existing bin/ symlink at the build being installed
// (the directory of exe), like bootstrap's `ln -sfn`. A name that is absent, a
// regular file or whose sibling build file is missing is left alone.
func relinkBins(w io.Writer, exe string) error {
	dir, err := binLinkDirFn()
	if err != nil || dir == "" {
		return err
	}
	if !underDir(exe, libRootFn()) {
		fmt.Fprintf(w, "warning: %s is not under the bootstrap lib root (%s); the bin/ symlinks are left as they are\n", exe, libRootFn())
		return nil
	}
	for _, n := range binLinkNames {
		link := filepath.Join(dir, n)
		st, err := os.Lstat(link)
		if err != nil || st.Mode()&os.ModeSymlink == 0 {
			continue
		}
		target := filepath.Join(filepath.Dir(exe), n)
		if n == binLinkNames[0] {
			target = exe // the daemon itself, whatever the file is called
		}
		if _, err := os.Lstat(target); err != nil {
			continue
		}
		if err := linkTargetSafe(target); err != nil {
			fmt.Fprintf(w, "warning: not repointing %s: %v\n", link, err)
			continue
		}
		if cur, err := os.Readlink(link); err == nil && cur == target {
			continue
		}
		if sameFile(link, target) {
			continue
		}
		tmp := link + ".new"
		os.Remove(tmp)
		if err := os.Symlink(target, tmp); err != nil {
			return err
		}
		if err := os.Rename(tmp, link); err != nil {
			os.Remove(tmp)
			return err
		}
		fmt.Fprintf(w, "repointed %s at %s\n", link, target)
	}
	return nil
}

// unitVersionCheck compares the build named by the unit's Condition, its
// ExecStart and the claude-memsync bin/ symlink. No unit (or not Linux) and an
// absent link are not failures.
func unitVersionCheck() checkResult {
	r := checkResult{ID: unitVersionID, OK: true}
	upath, content, ok := readUserUnit()
	if !ok {
		r.Detail = "no systemd user unit"
		return r
	}
	if c, found := currentCondition(content); found {
		r.Pieces = append(r.Pieces, unitPiece{"Condition", c})
	}
	if cur, found := currentExecStart(content); found {
		if exe, _, parsed := parseExecStart(cur); parsed {
			r.Pieces = append(r.Pieces, unitPiece{"ExecStart", exe})
		} else {
			r.OK = false
			// Never echo the line: a hand edit may carry secrets.
			r.Detail = "ExecStart in " + upath + " could not be parsed\n"
		}
	}
	if dir, err := binLinkDirFn(); err == nil && dir != "" {
		link := filepath.Join(dir, "claude-memsync")
		if st, err := os.Lstat(link); err == nil {
			p := link
			if st.Mode()&os.ModeSymlink != 0 {
				if t, err := filepath.EvalSymlinks(link); err == nil {
					p = t
				} else if t, err := os.Readlink(link); err == nil {
					p = t
				}
			}
			r.Pieces = append(r.Pieces, unitPiece{"symlink", p})
		}
	}
	for _, p := range r.Pieces[min(1, len(r.Pieces)):] {
		if !sameFile(p.Path, r.Pieces[0].Path) {
			r.OK = false
		}
	}
	if !r.OK {
		var b strings.Builder
		b.WriteString(r.Detail)
		b.WriteString("the pieces that say which build runs disagree:\n")
		for _, p := range r.Pieces {
			fmt.Fprintf(&b, "  %s -> %s\n", p.Name, p.Path)
		}
		b.WriteString("fix: re-run `claude-memsync install` from the build you want")
		r.Detail = b.String()
	}
	return r
}

// reportUnitVersion prints a failed check to w and returns r.OK.
func reportUnitVersion(w io.Writer, r checkResult) bool {
	if !r.OK {
		fmt.Fprintf(w, "FAIL %s: %s\n", r.ID, r.Detail)
	}
	return r.OK
}
