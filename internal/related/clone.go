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
	"strings"
	"time"

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
}

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

func (c *ReadClone) gitIn(ctx context.Context, useStore bool, stdin []byte, args ...string) ([]byte, error) {
	full := []string{"-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false",
		"-c", "protocol.ext.allow=never", "-c", "fetch.fsckObjects=true"}
	if useStore {
		full = append(full, "--git-dir="+c.store)
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	proc.HideWindow(cmd)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND="+sshCommand())
	cmd.WaitDelay = 2 * time.Second
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
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
// the host's smudge filters on content the remote chose.
func (c *ReadClone) export(ctx context.Context) error {
	listing, err := c.git(ctx, true, "ls-tree", "-r", "-z", "--full-tree", refHead)
	if err != nil {
		return err
	}
	type ent struct{ oid, path string }
	var ents []ent
	var oids bytes.Buffer
	for _, rec := range strings.Split(string(listing), "\x00") {
		tab := strings.IndexByte(rec, '\t')
		if tab < 0 {
			continue
		}
		f := strings.Fields(rec[:tab])
		if len(f) != 3 || f[1] != "blob" || f[0] == "120000" {
			continue // symlinks and submodules are not exported
		}
		p, ok := safeRel(rec[tab+1:])
		if !ok {
			continue
		}
		ents = append(ents, ent{f[2], p})
		oids.WriteString(f[2] + "\n")
	}
	var blobs []byte
	if len(ents) > 0 {
		if blobs, err = c.gitIn(ctx, true, oids.Bytes(), "cat-file", "--batch"); err != nil {
			return err
		}
	}
	tmp, err := os.MkdirTemp(c.root, ".tmp-"+c.name+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	br := bufio.NewReader(bytes.NewReader(blobs))
	for _, e := range ents {
		var oid, typ string
		var size int64
		line, err := br.ReadString('\n')
		if err != nil {
			return fmt.Errorf("reading %s: %w", e.path, err)
		}
		if _, err := fmt.Sscanf(line, "%s %s %d", &oid, &typ, &size); err != nil || typ != "blob" || size > maxFile {
			return fmt.Errorf("unexpected object for %s: %q", e.path, strings.TrimSpace(line))
		}
		body := make([]byte, size)
		if _, err := io.ReadFull(br, body); err != nil {
			return err
		}
		br.ReadByte() // the newline after the body
		dst := filepath.Join(tmp, filepath.FromSlash(e.path))
		if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(dst, body, 0600); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(c.tree); err != nil {
		return err
	}
	return os.Rename(tmp, c.tree)
}

const maxFile = 64 << 20

// safeRel cleans a repo path and refuses one that leaves the tree or touches
// a .git directory.
func safeRel(p string) (string, bool) {
	name := path.Clean(p)
	if name == "." || path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
		return "", false
	}
	for _, seg := range strings.Split(name, "/") {
		if strings.EqualFold(seg, ".git") {
			return "", false
		}
	}
	return name, true
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
