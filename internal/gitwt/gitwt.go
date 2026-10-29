// Package gitwt is a thin wrapper around the system `git` binary, scoped to a
// single working tree (the sync repo). We shell out rather than use go-git
// because we need full git features — specifically custom merge drivers,
// which go-git does not implement.
package gitwt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// NoAutoMaintenance turns off the auto gc and auto maintenance a commit, fetch,
// merge or rebase would start. git runs that as `maintenance run --auto
// --detach`, which setsid()s out of proc's process group, so a deadline or
// timeout could not kill it. With the trigger off nothing runs, detached or
// not; the daemon runs its own bounded maintenance instead (see the sync
// package). Do not use these for that explicit run: `--auto` reads gc.auto.
var NoAutoMaintenance = []string{"-c", "gc.auto=0", "-c", "maintenance.auto=false"}

// Maintain runs git's own auto housekeeping (`gc --auto`) in the foreground
// and under ctx: cancelling kills git's whole process group. It deliberately
// omits NoAutoMaintenance, because --auto reads gc.auto and would do nothing.
// Callers hold the sync dir's lock.
func (r *Repo) Maintain(ctx context.Context) error {
	cmd := proc.GroupedCommandContext(ctx, "git", "-C", r.Dir,
		"-c", "gc.autoDetach=false", "-c", "maintenance.autoDetach=false", "gc", "--auto", "--quiet")
	cmd.Env = repoEnv(nil)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := proc.Run(cmd); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("git gc --auto: %w", ctx.Err())
		}
		return fmt.Errorf("git gc --auto: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// quietReceivePack keeps the receive-pack of a push to a local path from
// starting auto maintenance. A local transport drops -c and GIT_CONFIG_COUNT
// before it starts receive-pack (verified on git 2.56), so only the command
// line reaches it. Remote (ssh, https) receive-packs run on the server and are
// not ours to configure, so they get no such option.
const quietReceivePack = "--receive-pack=git -c receive.autogc=false -c gc.auto=0 -c maintenance.auto=false receive-pack"

// pushesToLocalPath reports whether the remote a push (its args after "push")
// names is a path or file:// URL on this machine.
func (r *Repo) pushesToLocalPath(args []string) bool {
	remote := ""
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			remote = a
			break
		}
	}
	if remote == "" {
		return false
	}
	out, err := r.run(nil, nil, "remote", "get-url", "--push", remote)
	if err != nil {
		return false
	}
	return isLocalURL(strings.TrimSpace(out))
}

func isLocalURL(u string) bool {
	if strings.HasPrefix(u, "file://") {
		return true
	}
	if strings.Contains(u, "://") {
		return false
	}
	if len(u) >= 3 && u[1] == ':' && (u[2] == '\\' || u[2] == '/') { // C:\repo
		return true
	}
	// scp-like host:path has a colon before any slash.
	c, sl := strings.Index(u, ":"), strings.IndexAny(u, "/")
	return c < 0 || (sl >= 0 && sl < c)
}

// Run executes `git -C <dir> <args...>` and returns stdout. Stderr is
// included in the returned error on non-zero exit.
func (r *Repo) Run(args ...string) (string, error) {
	return r.RunStdin(nil, args...)
}

// RunStdin is Run with stdin fed to git. Use it, with --pathspec-from-file=-
// or `cat-file --batch`, instead of putting a long or untrusted list of paths
// on the command line.
func (r *Repo) RunStdin(stdin []byte, args ...string) (string, error) {
	return r.run(nil, stdin, args...)
}

// run is RunStdin with extra environment variables for git (KEY=VALUE).
func (r *Repo) run(env []string, stdin []byte, args ...string) (string, error) {
	full := append([]string{"-C", r.Dir}, NoAutoMaintenance...)
	sent := args
	if len(args) > 0 && args[0] == "push" && r.pushesToLocalPath(args[1:]) {
		sent = append([]string{"push", quietReceivePack}, args[1:]...)
	}
	full = append(full, sent...)
	cmd := proc.Command("git", full...)
	cmd.Env = repoEnv(env)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := proc.Run(cmd); err != nil {
		return stdout.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// RunEnv is RunStdin with extra environment variables (KEY=VALUE), e.g. a
// private GIT_INDEX_FILE.
func (r *Repo) RunEnv(env []string, stdin []byte, args ...string) (string, error) {
	return r.run(env, stdin, args...)
}

// RunClean is Run; kept for callers that want to say the output must not
// depend on the environment (run already drops git's repository-location
// variables, see repoEnv).
func (r *Repo) RunClean(args ...string) (string, error) { return r.run(nil, nil, args...) }

// repoEnv is the environment for git: the caller's, minus the variables that
// would redirect git to another repository than the -C directory.
func repoEnv(extra []string) []string {
	var env []string
	for _, e := range os.Environ() {
		k, _, _ := strings.Cut(e, "=")
		switch k {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
			"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_NAMESPACE", "GIT_PREFIX":
			continue
		}
		env = append(env, e)
	}
	return append(env, extra...)
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

// CloneNoCheckout is Clone without populating the work-tree (`clone -n`), so
// config that checkout depends on (filters) can be set first.
func (r *Repo) CloneNoCheckout(url string) error {
	cmd := exec.Command("git", "clone", "-n", url, r.Dir)
	proc.HideWindow(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git clone -n %s: %w: %s", url, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// HasHead reports whether HEAD points at a commit (false in an empty repo).
func (r *Repo) HasHead() bool {
	_, err := r.Run("rev-parse", "--verify", "--quiet", "HEAD")
	return err == nil
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

// DirtyPaths lists paths under dir (the whole work-tree when dir is "") that
// differ from HEAD or are untracked (ignored files excluded), one entry per
// path.
func (r *Repo) DirtyPaths(dir string) ([]string, error) {
	args := []string{"status", "--porcelain", "-z", "--no-renames", "--untracked-files=all"}
	if dir != "" {
		args = append(args, "--", Literal(dir))
	}
	out, err := r.Run(args...)
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

// HeadFiles returns the set of files under dir (the whole tree when dir is "")
// in HEAD (empty when HEAD is unborn).
func (r *Repo) HeadFiles(dir string) (map[string]bool, error) {
	set := map[string]bool{}
	if _, err := r.Run("rev-parse", "--verify", "--quiet", "HEAD"); err != nil {
		return set, nil
	}
	args := []string{"ls-tree", "-r", "--name-only", "-z", "HEAD"}
	if dir != "" {
		args = append(args, "--", Literal(dir))
	}
	out, err := r.Run(args...)
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

// Fetch does `git fetch origin <branch>`, which also moves origin/<branch>.
// Ref logging is forced on, whatever the user's global config says: the
// rebase's --fork-point reads origin/<branch>'s reflog.
func (r *Repo) Fetch(branch string) error {
	_, err := r.Run("-c", "core.logAllRefUpdates=true", "fetch", "origin", branch)
	return err
}

// RebaseOnto does `git rebase --fork-point --autostash origin/<branch>`, the
// second half of Pull. --fork-point is what `git pull --rebase` uses: without
// it, commits a force-push removed upstream (say, a purged secret) would be
// replayed and pushed back. The rebase's reflog entries are named
// reflogAction, so that the reflog says who started it (see ReflogSubjects);
// ref logging is forced on so those entries exist, and the merge backend so
// they have the "(start)" form whatever rebase.backend the user set.
func (r *Repo) RebaseOnto(branch, reflogAction string) error {
	_, err := r.run([]string{"GIT_REFLOG_ACTION=" + reflogAction}, nil,
		"-c", "core.logAllRefUpdates=true", "-c", "rebase.backend=merge",
		"rebase", "--fork-point", "--autostash", "origin/"+branch)
	return err
}

// ReflogSubjects returns the subjects of the newest n entries of HEAD's
// reflog, newest first. A rebase's start is recorded as
// "<GIT_REFLOG_ACTION> (start): checkout <upstream>".
func (r *Repo) ReflogSubjects(n int) ([]string, error) {
	out, err := r.Run("reflog", "-n", strconv.Itoa(n), "--format=%gs", "HEAD")
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimRight(out, "\n"), "\n"), nil
}

// RebaseInProgress reports whether a rebase is stopped in this work-tree, as a
// conflicting `pull --rebase` leaves it.
func (r *Repo) RebaseInProgress() bool {
	return r.gitPathExists("rebase-merge") || r.gitPathExists("rebase-apply")
}

// MergeInProgress reports whether a merge is unfinished (MERGE_HEAD exists).
func (r *Repo) MergeInProgress() bool {
	return r.gitPathExists("MERGE_HEAD")
}

// gitPathExists reports whether name exists in the git directory.
func (r *Repo) gitPathExists(name string) bool {
	_, err := os.Stat(r.gitPath(name))
	return err == nil
}

// gitPath is the absolute path of name in the git directory; "" if git cannot
// say.
func (r *Repo) gitPath(name string) string {
	out, err := r.Run("rev-parse", "--git-path", name)
	p := strings.TrimSpace(out)
	if err != nil || p == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(r.Dir, p) // relative to the -C directory
	}
	return p
}

// HeadDetached reports whether HEAD is not on a branch, as in the middle of a
// rebase or after checking out a commit.
func (r *Repo) HeadDetached() bool {
	_, err := r.Run("symbolic-ref", "-q", "HEAD")
	return err != nil
}

// HasUnmerged reports whether the index holds unmerged (conflicted) entries.
func (r *Repo) HasUnmerged() bool {
	out, err := r.Run("ls-files", "-u", "-z")
	return err == nil && out != ""
}

// RebaseAbort abandons the rebase in progress, restoring the branch and the
// work-tree to where the pull found them.
func (r *Repo) RebaseAbort() error {
	_, err := r.Run("rebase", "--abort")
	return err
}

// MergeAbort abandons the merge in progress.
func (r *Repo) MergeAbort() error {
	_, err := r.Run("merge", "--abort")
	return err
}

// IsAncestor reports whether commit a is an ancestor of (or the same as) b.
func (r *Repo) IsAncestor(a, b string) (bool, error) {
	_, err := r.Run("merge-base", "--is-ancestor", a, b)
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil // exit 1 = not an ancestor
	}
	return false, err
}

// UnmergedPaths lists the paths with unresolved conflicts.
func (r *Repo) UnmergedPaths() ([]string, error) {
	out, err := r.Run("diff", "--name-only", "--diff-filter=U", "-z")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
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

// UnmergedEntry is one index entry of a conflicted path: its stage (1 base,
// 2 the branch being rebased onto, 3 the commit being applied) and blob id.
type UnmergedEntry struct {
	Path  string
	Stage int
	Oid   string
}

// UnmergedEntries lists the conflicted index entries.
func (r *Repo) UnmergedEntries() ([]UnmergedEntry, error) {
	out, err := r.Run("ls-files", "-u", "-z")
	if err != nil {
		return nil, err
	}
	var es []UnmergedEntry
	for _, rec := range strings.Split(out, "\x00") {
		// "<mode> <oid> <stage>\t<path>"
		tab := strings.IndexByte(rec, '\t')
		if tab < 0 {
			continue
		}
		f := strings.Fields(rec[:tab])
		if len(f) != 3 {
			continue
		}
		st, err := strconv.Atoi(f[2])
		if err != nil {
			continue
		}
		es = append(es, UnmergedEntry{Path: rec[tab+1:], Stage: st, Oid: f[1]})
	}
	return es, nil
}

// RebaseContinue continues the stopped rebase without opening an editor. env
// is extra KEY=VALUE settings (the reflog action, so the reflog still names the
// daemon's pull).
func (r *Repo) RebaseContinue(env ...string) error {
	_, err := r.run(append([]string{"GIT_EDITOR=true"}, env...), nil, "-c", "core.editor=true", "rebase", "--continue")
	return err
}
