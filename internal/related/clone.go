package related

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/MarimerLLC/claude-utils/internal/proc"
)

// DefaultFetchTimeout bounds one fetch.
const DefaultFetchTimeout = 60 * time.Second

// disabledPush is the push URL of every read clone.
const disabledPush = "disabled://read-only-clone"

// refHead is where a fetch lands the remote's HEAD.
const refHead = "refs/fetched/head"

// ReadClone is a fetch-only copy of a related repo. It is a type of its own
// and deliberately has no Push, Commit or work-tree method: nothing in the
// program can send anything to the remote through it, and its push URL is
// disabled as a second belt.
//
// Nothing fetched is ever checked out, so no filter, hook or attribute that
// arrives with the content can run on this host. The remote's objects live in
// a bare store (<root>/.bare/<repo>.git); the files a session reads are a
// plain export of the tip (git archive, which runs no smudge filters) at
// <root>/<repo>, rewritten on every fetch.
type ReadClone struct {
	name    string
	root    string
	store   string
	tree    string
	url     string
	Timeout time.Duration // per git command; zero means DefaultFetchTimeout
	// MaxBytes caps the files exported from one fetch (zero: DefaultMaxBytes).
	// A tip larger than that is refused and the old copy is kept.
	MaxBytes int64
}

// DefaultMaxBytes is the default cap on one clone's exported files.
const DefaultMaxBytes int64 = 64 << 20

// URLRepoName is the repository name a clone URL points at: its last path
// segment (also for scp-style ssh aliases), lower-cased, without ".git".
func URLRepoName(url string) string {
	u := strings.TrimRight(strings.TrimSpace(url), "/\\")
	u = strings.TrimSuffix(u, ".git")
	if i := strings.LastIndexAny(u, "/:\\"); i >= 0 {
		u = u[i+1:]
	}
	return strings.ToLower(u)
}

// NewReadClone describes the clone of repo under root. A non-empty url must
// point at a repository of that name: a relation to one repo can never fetch
// another.
func NewReadClone(root, repo, url string) (*ReadClone, error) {
	if !nameRe.MatchString(repo) {
		return nil, fmt.Errorf("%q is not a mem-* repo name", repo)
	}
	if url != "" {
		if got := URLRepoName(url); got != repo {
			return nil, fmt.Errorf("the URL for %s names the repository %q; refusing to fetch it", repo, got)
		}
	}
	return &ReadClone{name: repo, root: root, store: filepath.Join(root, ".bare", repo+".git"), tree: filepath.Join(root, repo), url: url}, nil
}

// Name is the related repo's name.
func (c *ReadClone) Name() string { return c.name }

// Dir is where the files a session may read are.
func (c *ReadClone) Dir() string { return c.tree }

// Exists reports a clone that has been fetched at least once.
func (c *ReadClone) Exists() bool {
	if _, err := os.Stat(filepath.Join(c.store, "HEAD")); err != nil {
		return false
	}
	_, err := os.Stat(c.tree)
	return err == nil
}

func (c *ReadClone) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return DefaultFetchTimeout
}

// sshCommand is the host's ssh command with BatchMode added, so a key
// passphrase or host-key question fails instead of waiting for a person.
func sshCommand() string {
	base := os.Getenv("GIT_SSH_COMMAND")
	if base == "" {
		if out, err := exec.Command("git", "config", "--get", "core.sshCommand").Output(); err == nil {
			base = strings.TrimSpace(string(out))
		}
	}
	if base == "" {
		base = os.Getenv("GIT_SSH")
	}
	if base == "" {
		base = "ssh"
	}
	return base + " -o BatchMode=yes"
}

// git runs git against the bare store with every automatic behaviour off.
func (c *ReadClone) git(ctx context.Context, useStore bool, args ...string) ([]byte, error) {
	return c.gitIn(ctx, useStore, nil, args...)
}

func (c *ReadClone) command(ctx context.Context, useStore bool, args ...string) *exec.Cmd {
	full := []string{"-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false",
		"-c", "protocol.ext.allow=never",
		// The host's config must not relax object checks on content the remote chose.
		"-c", "fetch.fsckObjects=true", "-c", "transfer.fsckObjects=true",
		"-c", "fetch.fsck.hasDotgit=error", "-c", "fsck.hasDotgit=error"}
	if useStore {
		full = append(full, "--git-dir="+c.store)
	}
	full = append(full, args...)
	cmd := proc.GroupedCommandContext(ctx, "git", full...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if len(args) > 0 && args[0] == "fetch" {
		// Only a fetch talks to the remote; looking the ssh command up costs a
		// git process, which the hook's calls must not pay.
		cmd.Env = append(cmd.Env, "GIT_SSH_COMMAND="+sshCommand())
	}
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

func (c *ReadClone) gitIn(ctx context.Context, useStore bool, stdin []byte, args ...string) ([]byte, error) {
	cmd := c.command(ctx, useStore, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := proc.Run(cmd); err != nil {
		if ctx.Err() != nil {
			return out.Bytes(), fmt.Errorf("git %s: %w", args[0], ctx.Err())
		}
		return out.Bytes(), fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// Fetch brings the clone up to the remote's tip, creating it the first time.
// It fails, leaving the old copy in place, when the configured URL is not the
// one the store was made for.
func (c *ReadClone) Fetch() (err error) {
	if c.url == "" {
		return errors.New("no remote URL configured")
	}
	defer func() {
		if err != nil {
			c.mark("failed", true)
		} else {
			c.mark("failed", false)
			c.stamp()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout())
	defer cancel()
	if _, serr := os.Stat(filepath.Join(c.store, "HEAD")); serr != nil {
		if err := os.MkdirAll(filepath.Dir(c.store), 0700); err != nil {
			return err
		}
		if _, err := c.git(ctx, false, "init", "--bare", "-q", c.store); err != nil {
			return err
		}
		if _, err := c.git(ctx, true, "config", "remote.origin.url", c.url); err != nil {
			return err
		}
	} else {
		got, err := c.git(ctx, true, "config", "--get", "remote.origin.url")
		if err != nil || strings.TrimSpace(string(got)) != c.url {
			return fmt.Errorf("the clone of %s was made from %q, not the configured URL; delete %s to refetch", c.name, strings.TrimSpace(string(got)), c.store)
		}
	}
	if _, err := c.git(ctx, true, "config", "remote.origin.pushurl", disabledPush); err != nil {
		return err
	}
	if _, err := c.git(ctx, true, "fetch", "--no-tags", "--no-recurse-submodules", "origin", "+HEAD:"+refHead); err != nil {
		return err
	}
	return c.export(ctx)
}

// export replaces the readable tree with the fetched tip. It reads the blobs
// as they are stored (ls-tree and cat-file): git archive and checkout would run
// the host's smudge filters on content the remote chose. Blobs stream to their
// files one at a time; the total is checked against MaxBytes before anything
// is written, and again while writing.
func (c *ReadClone) export(ctx context.Context) error {
	limit := c.MaxBytes
	if limit <= 0 {
		limit = DefaultMaxBytes
	}
	listing, err := c.git(ctx, true, "ls-tree", "-r", "-l", "-z", "--full-tree", refHead)
	if err != nil {
		return err
	}
	type ent struct {
		oid, path string
		size      int64
	}
	var ents []ent
	var oids bytes.Buffer
	var total int64
	for _, rec := range strings.Split(string(listing), "\x00") {
		tab := strings.IndexByte(rec, '\t')
		if tab < 0 {
			continue
		}
		f := strings.Fields(rec[:tab])
		if len(f) != 4 || f[1] != "blob" || f[0] == "120000" {
			continue // symlinks and submodules are not exported
		}
		p, ok := safeRel(rec[tab+1:])
		if !ok {
			continue
		}
		size, err := strconv.ParseInt(f[3], 10, 64)
		if err != nil {
			return fmt.Errorf("unexpected listing for %s", p)
		}
		if total += size; total > limit {
			return fmt.Errorf("%s is larger than the %d byte limit for a read clone; not exported", c.name, limit)
		}
		ents = append(ents, ent{f[2], p, size})
		oids.WriteString(f[2] + "\n")
	}
	tmp, err := os.MkdirTemp(c.root, ".tmp-"+c.name+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if len(ents) > 0 {
		cmd := c.command(ctx, true, "cat-file", "--batch")
		cmd.Stdin = bytes.NewReader(oids.Bytes())
		var errb bytes.Buffer
		cmd.Stderr = &errb
		pipe, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		if err := cmd.Start(); err != nil {
			return err
		}
		werr := writeBlobs(tmp, bufio.NewReader(pipe), len(ents), func(i int) (string, int64) { return ents[i].path, ents[i].size })
		if werr != nil {
			cmd.Process.Kill()
		}
		cerr := cmd.Wait()
		if werr != nil {
			return werr
		}
		if cerr != nil {
			return fmt.Errorf("git cat-file: %w: %s", cerr, strings.TrimSpace(errb.String()))
		}
	}
	if err := os.RemoveAll(c.tree); err != nil {
		return err
	}
	return os.Rename(tmp, c.tree)
}

// writeBlobs reads n cat-file --batch records and streams each into its file
// under dir, checking each header against the size ls-tree announced.
func writeBlobs(dir string, br *bufio.Reader, n int, entry func(i int) (string, int64)) error {
	for i := 0; i < n; i++ {
		rel, want := entry(i)
		line, err := br.ReadString('\n')
		if err != nil {
			return fmt.Errorf("reading %s: %w", rel, err)
		}
		var oid, typ string
		var size int64
		if _, err := fmt.Sscanf(line, "%s %s %d", &oid, &typ, &size); err != nil || typ != "blob" || size != want {
			return fmt.Errorf("unexpected object for %s: %q", rel, strings.TrimSpace(line))
		}
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
			return err
		}
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, cerr := io.CopyN(f, br, size)
		if err := f.Close(); cerr == nil {
			cerr = err
		}
		if cerr != nil {
			return cerr
		}
		br.ReadByte() // the newline after the body
	}
	return nil
}

var driveRe = regexp.MustCompile(`^[A-Za-z]:`)

// safeRel cleans a repo path and refuses one that leaves the tree, is
// absolute, uses backslashes, or has a segment that is, or could be taken by
// some file system for, a .git directory. fsck on fetch is the first layer;
// this is the second.
func safeRel(p string) (string, bool) {
	if p == "" || strings.ContainsAny(p, "\\\x00") || strings.HasPrefix(p, "/") || driveRe.MatchString(p) {
		return "", false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || isDotgitAlias(seg) {
			return "", false
		}
	}
	name := path.Clean(p)
	if name == "." || path.IsAbs(name) {
		return "", false
	}
	return name, true
}

// isDotgitAlias reports a path segment that names .git on some file system:
// any case, trailing dots or spaces (NTFS), the short name git~N, an NTFS
// stream suffix, or invisible Unicode (HFS+) mixed in.
func isDotgitAlias(seg string) bool {
	var b strings.Builder
	for _, r := range seg {
		if unicode.Is(unicode.Cf, r) {
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	s := b.String()
	if i := strings.IndexByte(s, ':'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimRight(s, ". ")
	if s == ".git" {
		return true
	}
	if rest, ok := strings.CutPrefix(s, "git~"); ok && rest != "" {
		return strings.Trim(rest, "0123456789") == ""
	}
	return false
}

func (c *ReadClone) stateFile(kind string) string {
	return filepath.Join(c.root, ".state", kind, c.name)
}

func (c *ReadClone) mark(kind string, on bool) {
	p := c.stateFile(kind)
	if !on {
		os.Remove(p)
		return
	}
	if os.MkdirAll(filepath.Dir(p), 0700) == nil {
		_ = os.WriteFile(p, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0600)
	}
}

func (c *ReadClone) stamp() {
	p := c.stateFile("ok")
	if os.MkdirAll(filepath.Dir(p), 0700) == nil {
		_ = os.WriteFile(p, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0600)
	}
}

// Stale reports a clone whose last fetch failed, and since when it has had
// no good fetch ("never" if there was none).
func (c *ReadClone) Stale() (since string, stale bool) {
	if _, err := os.Stat(c.stateFile("failed")); err != nil {
		return "", false
	}
	b, err := os.ReadFile(c.stateFile("ok"))
	if err != nil || strings.TrimSpace(string(b)) == "" {
		return "never", true
	}
	return strings.TrimSpace(string(b)), true
}

// Head is the fetched commit, or "" when there is none.
func (c *ReadClone) Head() string {
	out, err := c.git(context.Background(), true, "rev-parse", "--verify", "-q", refHead)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// FileChange is one path that differs between two commits of the clone.
type FileChange struct {
	Status string // added, changed or deleted
	Path   string // relative to the clone
}

// ChangedSince lists the files that changed between commit since and the
// fetched tip. A since that no longer exists yields nil, false: the caller
// re-baselines.
func (c *ReadClone) ChangedSince(since string) ([]FileChange, bool) {
	ctx := context.Background()
	if _, err := c.git(ctx, true, "cat-file", "-e", since+"^{commit}"); err != nil {
		return nil, false
	}
	out, err := c.git(ctx, true, "diff", "--name-status", "-z", "--no-renames", "--no-ext-diff", "--no-textconv", since, refHead)
	if err != nil {
		return nil, false
	}
	var res []FileChange
	parts := strings.Split(string(out), "\x00")
	for i := 0; i+1 < len(parts); i += 2 {
		st := "changed"
		switch parts[i] {
		case "A":
			st = "added"
		case "D":
			st = "deleted"
		}
		res = append(res, FileChange{st, parts[i+1]})
	}
	return res, true
}
