// Package gitwt is a thin wrapper around the system `git` binary, scoped to a
// single working tree (the sync repo). We shell out rather than use go-git
// because we need full git features — specifically custom merge drivers,
// which go-git does not implement.
package gitwt

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/proc"
)

// Repo represents a local git working tree at a fixed path.
type Repo struct {
	Dir string
}

// New constructs a Repo for the given working directory. The directory does
// not need to exist yet; pass it through Init or Clone first.
func New(dir string) *Repo { return &Repo{Dir: dir} }

// Run executes `git -C <dir> <args...>` and returns stdout. Stderr is
// included in the returned error on non-zero exit.
func (r *Repo) Run(args ...string) (string, error) {
	return r.RunStdin(nil, args...)
}

// RunStdin is Run with stdin fed to git. Use it, with --pathspec-from-file=-
// or `cat-file --batch`, instead of putting a long or untrusted list of paths
// on the command line.
func (r *Repo) RunStdin(stdin []byte, args ...string) (string, error) {
	full := append([]string{"-C", r.Dir}, args...)
	cmd := exec.Command("git", full...)
	proc.HideWindow(cmd)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// IsRepo reports whether r.Dir is inside an existing git work-tree.
func (r *Repo) IsRepo() bool {
	_, err := r.Run("rev-parse", "--is-inside-work-tree")
	return err == nil
}

// Clone clones url into r.Dir. r.Dir must not already exist or must be empty.
func (r *Repo) Clone(url string) error {
	cmd := exec.Command("git", "clone", url, r.Dir)
	proc.HideWindow(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git clone %s: %w: %s", url, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Init runs `git init` in r.Dir. The directory is created if missing.
func (r *Repo) Init(defaultBranch string) error {
	cmd := exec.Command("git", "init", "-b", defaultBranch, r.Dir)
	proc.HideWindow(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git init: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// SetRemote sets origin to url, replacing any existing origin.
func (r *Repo) SetRemote(url string) error {
	if _, err := r.Run("remote", "remove", "origin"); err != nil {
		// ignore: origin may not exist
	}
	_, err := r.Run("remote", "add", "origin", url)
	return err
}

// ConfigSet writes a key=value pair to the local repo config.
func (r *Repo) ConfigSet(key, value string) error {
	_, err := r.Run("config", key, value)
	return err
}

// AddAll stages all changes (including deletions) under the work-tree.
func (r *Repo) AddAll() error {
	_, err := r.Run("add", "-A")
	return err
}

// Literal marks p as a literal path (no globs, no magic) for use in a git
// pathspec. Every path this package hands to git goes through it, so a memory
// file called "[ab].md" or ":(top)x" cannot widen into other files.
func Literal(p string) string { return ":(literal)" + p }

// pathspecFile encodes paths for --pathspec-from-file=- --pathspec-file-nul.
func pathspecFile(specs []string) []byte {
	return []byte(strings.Join(specs, "\x00") + "\x00")
}

// withPathspecs runs `git <args> --pathspec-from-file=- --pathspec-file-nul`
// with specs on stdin, which has no argument-length limit and needs no
// quoting.
func (r *Repo) withPathspecs(specs []string, args ...string) error {
	args = append(args, "--pathspec-from-file=-", "--pathspec-file-nul")
	_, err := r.RunStdin(pathspecFile(specs), args...)
	return err
}

// StagedChange is one entry from `git diff --cached --raw`.
type StagedChange struct {
	Status byte   // 'A' added, 'M' modified, 'D' deleted, ...
	Path   string // slash-separated, relative to the work-tree root
	Mode   string // new mode, e.g. "100644"; "160000" is a submodule
	Oid    string // new blob id (all zeros for a deletion)
}

// StagedChanges lists what is staged relative to HEAD. Renames are reported as
// a delete plus an add so callers never see a status with two paths.
func (r *Repo) StagedChanges() ([]StagedChange, error) {
	out, err := r.Run("diff", "--cached", "--raw", "--no-abbrev", "--no-renames", "-z")
	if err != nil {
		return nil, err
	}
	// ":<old mode> <new mode> <old oid> <new oid> <status>\0<path>\0"
	fields := strings.Split(out, "\x00")
	var changes []StagedChange
	for i := 0; i+1 < len(fields); i += 2 {
		meta := strings.Fields(fields[i])
		if len(meta) != 5 {
			break
		}
		changes = append(changes, StagedChange{Status: meta[4][0], Path: fields[i+1], Mode: meta[1], Oid: meta[3]})
	}
	return changes, nil
}

// BlobContents returns the content of each blob id, in order, using a single
// `git cat-file --batch` process rather than one `git show` per file.
func (r *Repo) BlobContents(oids []string) ([][]byte, error) {
	if len(oids) == 0 {
		return nil, nil
	}
	out, err := r.RunStdin([]byte(strings.Join(oids, "\n")+"\n"), "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	// Each reply is "<oid> <type> <size>\n<size bytes>\n".
	buf := []byte(out)
	res := make([][]byte, 0, len(oids))
	for _, oid := range oids {
		nl := bytes.IndexByte(buf, '\n')
		if nl < 0 {
			return nil, fmt.Errorf("cat-file: short output for %s", oid)
		}
		hdr := strings.Fields(string(buf[:nl]))
		if len(hdr) != 3 {
			return nil, fmt.Errorf("cat-file: %s: %s", oid, buf[:nl])
		}
		size, err := strconv.Atoi(hdr[2])
		if err != nil || nl+1+size > len(buf) {
			return nil, fmt.Errorf("cat-file: bad header for %s", oid)
		}
		res = append(res, buf[nl+1:nl+1+size])
		buf = buf[nl+1+size+1:]
	}
	return res, nil
}

// Unstage removes paths from the index, leaving the work-tree untouched.
// Paths are literal.
func (r *Repo) Unstage(paths ...string) error {
	if len(paths) == 0 {
		return nil
	}
	specs := make([]string, len(paths))
	for i, p := range paths {
		specs[i] = Literal(p)
	}
	return r.withPathspecs(specs, "reset", "-q")
}

// UnstageFold removes path from the index whatever its letter case.
func (r *Repo) UnstageFold(path string) error {
	return r.withPathspecs([]string{":(literal,icase)" + path}, "reset", "-q")
}

// UnstageAll empties the index (the work-tree is untouched).
func (r *Repo) UnstageAll() error {
	_, err := r.Run("reset", "-q")
	return err
}

// DirtyPaths lists paths under dir that differ from HEAD or are untracked
// (ignored files excluded), one entry per path.
func (r *Repo) DirtyPaths(dir string) ([]string, error) {
	out, err := r.Run("status", "--porcelain", "-z", "--no-renames", "--untracked-files=all", "--", Literal(dir))
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, e := range strings.Split(out, "\x00") {
		if len(e) >= 4 { // "XY path"
			paths = append(paths, e[3:])
		}
	}
	return paths, nil
}

// HeadFiles returns the set of files under dir in HEAD (empty when HEAD is
// unborn).
func (r *Repo) HeadFiles(dir string) (map[string]bool, error) {
	set := map[string]bool{}
	if _, err := r.Run("rev-parse", "--verify", "--quiet", "HEAD"); err != nil {
		return set, nil
	}
	out, err := r.Run("ls-tree", "-r", "--name-only", "-z", "HEAD", "--", Literal(dir))
	if err != nil {
		return nil, err
	}
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			set[p] = true
		}
	}
	return set, nil
}

// CheckoutHead restores paths from HEAD in both index and work-tree.
func (r *Repo) CheckoutHead(paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	specs := make([]string, len(paths))
	for i, p := range paths {
		specs[i] = Literal(p)
	}
	return r.withPathspecs(specs, "checkout", "-q", "HEAD")
}

// HasStagedChanges reports whether there is anything staged that would be
// included in a commit.
func (r *Repo) HasStagedChanges() (bool, error) {
	_, err := r.Run("diff", "--cached", "--quiet")
	if err == nil {
		return false, nil // exit 0 = no diff
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return true, nil // exit 1 = has diff (per git diff --quiet contract)
	}
	return false, err
}

// Commit creates a commit if anything is staged. Returns true if a commit was made.
func (r *Repo) Commit(message string) (bool, error) {
	has, err := r.HasStagedChanges()
	if err != nil {
		return false, err
	}
	if !has {
		return false, nil
	}
	_, err = r.Run("commit", "-m", message)
	return err == nil, err
}

// Pull does `git pull --rebase --autostash origin <branch>`.
func (r *Repo) Pull(branch string) error {
	_, err := r.Run("pull", "--rebase", "--autostash", "origin", branch)
	return err
}

// Push does `git push origin <branch>`. The first push from a new local repo
// uses --set-upstream automatically here.
func (r *Repo) Push(branch string) error {
	_, err := r.Run("push", "--set-upstream", "origin", branch)
	return err
}

// CurrentBranch returns the name of the currently checked-out branch.
func (r *Repo) CurrentBranch() (string, error) {
	out, err := r.Run("rev-parse", "--abbrev-ref", "HEAD")
	return strings.TrimSpace(out), err
}

// FetchOriginBranch returns whether the named branch exists on origin.
// Used to decide between "first push" and "pull then push" on init.
func (r *Repo) FetchOriginBranch(branch string) (bool, error) {
	sha, err := r.LsRemoteHead(branch)
	if err != nil {
		return false, err
	}
	return sha != "", nil
}

// LsRemoteHead asks origin for the current SHA of refs/heads/<branch>.
// Returns ("", nil) if origin has no such branch yet. This is a single
// network round-trip with no working-tree side effects — used as a cheap
// gate to skip full pull/push cycles when the remote hasn't moved.
func (r *Repo) LsRemoteHead(branch string) (string, error) {
	out, err := r.Run("ls-remote", "--heads", "origin", branch)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(out)
	if line == "" {
		return "", nil
	}
	// Format: "<sha>\trefs/heads/<branch>"
	if i := strings.IndexAny(line, " \t"); i > 0 {
		return line[:i], nil
	}
	return "", nil
}
