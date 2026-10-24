package related

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// ReadClone is a fetch-only clone of a related repo. It is a type of its own,
// and deliberately has no Push, Commit or work-tree writing method: nothing in
// the program can send anything to the remote through it. As a second belt the
// clone's push URL is set to an address that cannot be reached, so even a git
// command run by hand against it fails.
type ReadClone struct {
	name string
	dir  string
	url  string
	g    *gitwt.Repo
}

// disabledPush is the push URL of every read clone.
const disabledPush = "disabled://read-only-clone"

// NewReadClone describes the clone of repo under root, fetched from url.
func NewReadClone(root, repo, url string) (*ReadClone, error) {
	if !nameRe.MatchString(repo) {
		return nil, fmt.Errorf("%q is not a mem-* repo name", repo)
	}
	dir := filepath.Join(root, repo)
	return &ReadClone{name: repo, dir: dir, url: url, g: gitwt.New(dir)}, nil
}

// Name is the related repo's name.
func (c *ReadClone) Name() string { return c.name }

// Dir is where the clone's files are.
func (c *ReadClone) Dir() string { return c.dir }

// Exists reports a clone that has been fetched at least once.
func (c *ReadClone) Exists() bool {
	_, err := os.Stat(filepath.Join(c.dir, ".git"))
	return err == nil
}

// Fetch clones the repo on the first call and brings it up to the remote's
// tip after that. Local edits to the clone are discarded: it is not a place to
// write.
func (c *ReadClone) Fetch() error {
	if c.url == "" {
		return errors.New("no remote URL configured")
	}
	if !c.Exists() {
		if err := os.MkdirAll(filepath.Dir(c.dir), 0700); err != nil {
			return err
		}
		if err := c.g.Clone(c.url); err != nil {
			return err
		}
		if err := c.g.SetRemote(c.url); err != nil {
			return err
		}
	}
	if _, err := c.g.Run("config", "remote.origin.pushurl", disabledPush); err != nil {
		return err
	}
	if _, err := c.g.Run("fetch", "--prune", "origin"); err != nil {
		return err
	}
	branch, err := c.g.Run("symbolic-ref", "--short", "HEAD")
	if err != nil {
		return nil // empty or detached: nothing to move
	}
	branch = strings.TrimSpace(branch)
	if _, err := c.g.Run("rev-parse", "--verify", "-q", "origin/"+branch); err != nil {
		return nil // the remote has no commits yet
	}
	_, err = c.g.Run("reset", "--hard", "origin/"+branch)
	return err
}

// Head is the clone's current commit, or "" when it has none.
func (c *ReadClone) Head() string {
	out, err := c.g.Run("rev-parse", "--verify", "-q", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// FileChange is one path that differs between two commits of the clone.
type FileChange struct {
	Status string // added, changed or deleted
	Path   string // relative to the clone
}

// ChangedSince lists the files that changed between commit since and HEAD. A
// since that no longer exists yields nil, false: the caller re-baselines.
func (c *ReadClone) ChangedSince(since string) ([]FileChange, bool) {
	if _, err := c.g.Run("cat-file", "-e", since+"^{commit}"); err != nil {
		return nil, false
	}
	out, err := c.g.Run("diff", "--name-status", "-z", "--no-renames", since, "HEAD")
	if err != nil {
		return nil, false
	}
	var res []FileChange
	parts := strings.Split(out, "\x00")
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
