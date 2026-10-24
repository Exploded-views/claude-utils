package sync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// These tests cover the sync repo's HEAD moving outside a daemon cycle: the
// documented manual conflict resolution, or any `git pull` run by hand. The
// daemon's next pull is then a no-op, so without a catch-up nothing would
// copy the pulled files into ~/.claude, and Reconcile would let the stale
// Claude copies win and push them over the peer's edits.

// addPC is a second PC on the same origin: its own sync work-tree and Claude
// tree, filled from what origin holds.
func (f *pullFixture) addPC(host string) *pullFixture {
	f.t.Helper()
	root := f.t.TempDir()
	syncDir := filepath.Join(root, "sync")
	repo := gitwt.New(syncDir)
	if err := repo.Clone(f.origin); err != nil {
		f.t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		f.t.Fatal(err)
	}
	mustGit(f.t, repo, "config", "merge.claude-memory-index.name", "claude memory index merge")
	mustGit(f.t, repo, "config", "merge.claude-memory-index.driver", `"`+filepath.ToSlash(self)+`" %O %A %B %L %P`)
	claude := filepath.Join(root, "claude", "projects")
	roots := Roots{Claude: claude, Mirror: filepath.Join(syncDir, "projects")}
	if _, err := Reconcile(roots, nil); err != nil {
		f.t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.SyncDir = syncDir
	cfg.ClaudeProjectsDir = claude
	return &pullFixture{
		t: f.t, origin: f.origin, repo: repo, roots: roots,
		loop: &Loop{Cfg: cfg, Branch: "main", Hostname: host},
	}
}

// restart is a daemon start: Run does its startup cycle, then stops.
func (f *pullFixture) restart() {
	f.t.Helper()
	loop := &Loop{Cfg: f.loop.Cfg, Branch: f.loop.Branch, Hostname: f.loop.Hostname}
	flushed := make(chan error, 4)
	loop.OnFlush = func(_ bool, err error) { flushed <- err }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- loop.Run(ctx) }()
	select {
	case <-flushed:
	case <-time.After(20 * time.Second):
		f.t.Fatal("no startup flush")
	}
	cancel()
	if err := <-done; err != nil {
		f.t.Fatal(err)
	}
}

func (f *pullFixture) originFile(name string) string {
	f.t.Helper()
	out, err := gitwt.New(f.origin).Run("show", "main:projects/p1/memory/"+name)
	if err != nil {
		f.t.Fatalf("origin has no %s: %v", name, err)
	}
	return out
}

func (f *pullFixture) syncedHead() string {
	b, _ := os.ReadFile(filepath.Join(f.repo.Dir, ".state", "synced-head"))
	return strings.TrimSpace(string(b))
}

func (f *pullFixture) head() string {
	return strings.TrimSpace(mustGit(f.t, f.repo, "rev-parse", "HEAD"))
}

// resolveByHand is the documented procedure, with the daemon stopped: pull
// with autostash, fix each stopped commit, add, continue, until done.
func resolveByHand(t *testing.T, f *pullFixture, resolved map[string]string) {
	t.Helper()
	t.Setenv("GIT_EDITOR", "true")
	if _, err := f.repo.Run("pull", "--rebase", "--autostash", "origin", "main"); err == nil {
		t.Fatal("expected the pull to stop on the conflict again")
	}
	for i := 0; f.repo.RebaseInProgress(); i++ {
		if i > 5 {
			t.Fatal("rebase never finished")
		}
		paths, _ := f.repo.UnmergedPaths()
		for _, p := range paths {
			name := strings.TrimPrefix(p, "projects/p1/memory/")
			writeRepoFile(t, f.repo, p, resolved[name])
			mustGit(t, f.repo, "add", p)
		}
		mustGit(t, f.repo, "rebase", "--continue")
	}
}

// Two PCs edit the same lines; PC A's pull stops. The user resolves by hand in
// ~/.claudesync and restarts the daemon. Origin and both Claude trees end with
// the peer's other edit and the user's resolution.
func TestCatchUp_ManualResolutionThenRestartKeepsBothSides(t *testing.T) {
	a := newPullFixture(t, map[string]string{"clean.md": "a\nb\n", "other.md": "o\n"})
	captureLog(t)
	b := a.addPC("pc-b")
	a.loop.pullTick(a.repo, a.roots)
	b.loop.pullTick(b.repo, b.roots)

	b.editLocal("clean.md", "a\npeer\n")
	b.editLocal("other.md", "o\npeer edit\n")
	b.loop.pullTick(b.repo, b.roots)
	a.editLocal("clean.md", "a\nlocal\n")
	a.loop.pullTick(a.repo, a.roots)
	if inProgress(a) {
		t.Fatal("the conflicted rebase should have been aborted")
	}

	resolveByHand(t, a, map[string]string{"clean.md": "a\nboth\n"})
	a.restart()
	b.loop.pullTick(b.repo, b.roots)

	for _, pc := range []*pullFixture{a, b} {
		if got, _ := pc.claudeCopy("other.md"); got != "o\npeer edit\n" {
			t.Errorf("%s: other.md = %q", pc.loop.Hostname, got)
		}
		if got, _ := pc.claudeCopy("clean.md"); got != "a\nboth\n" {
			t.Errorf("%s: clean.md = %q", pc.loop.Hostname, got)
		}
	}
	if got := a.originFile("other.md"); got != "o\npeer edit\n" {
		t.Errorf("origin other.md = %q", got)
	}
	if got := a.originFile("clean.md"); got != "a\nboth\n" {
		t.Errorf("origin clean.md = %q", got)
	}
}

// The same through the pull tick, with no restart: HEAD moves between ticks.
func TestCatchUp_HandPullFastForwardPropagatesOnNextTick(t *testing.T) {
	f := newPullFixture(t, map[string]string{"clean.md": "a\nb\n", "other.md": "o\n"})
	captureLog(t)
	f.loop.pullTick(f.repo, f.roots)
	f.peerPush("other.md", "o\npeer edit\n")
	mustGit(t, f.repo, "pull", "-q", "--rebase", "origin", "main")

	f.loop.pullTick(f.repo, f.roots)
	if got, _ := f.claudeCopy("other.md"); got != "o\npeer edit\n" {
		t.Errorf("Claude other.md = %q", got)
	}
	if got := f.originFile("other.md"); got != "o\npeer edit\n" {
		t.Errorf("origin other.md reverted: %q", got)
	}
	if f.syncedHead() != f.head() {
		t.Errorf("record %q, HEAD %q", f.syncedHead(), f.head())
	}
}

// With no record (the first run after an upgrade) the record is set to HEAD and
// nothing is copied.
func TestCatchUp_NoRecordSetsHeadOnly(t *testing.T) {
	f := newPullFixture(t, map[string]string{"clean.md": "a\nb\n"})
	captureLog(t)
	f.peerPush("clean.md", "a\npeer\n")
	mustGit(t, f.repo, "pull", "-q", "--rebase", "origin", "main")
	if f.syncedHead() != "" {
		t.Fatal("unexpected record")
	}

	f.loop.startCycle(f.repo, f.roots)
	if f.syncedHead() != f.head() {
		t.Errorf("record %q, HEAD %q", f.syncedHead(), f.head())
	}
	if got, _ := f.claudeCopy("clean.md"); got != "a\nb\n" {
		t.Errorf("Claude copy changed without a record: %q", got)
	}
}

// A path that is dirty in the work-tree (held back, say) is the user's version:
// the catch-up leaves its Claude copy alone, and the rest is propagated.
func TestCatchUp_DirtyPathLeftAlone(t *testing.T) {
	f := newPullFixture(t, map[string]string{"held.md": "h\n", "other.md": "o\n"})
	captureLog(t)
	f.loop.pullTick(f.repo, f.roots)
	f.peerPush("held.md", "h\npeer\n")
	f.peerPush("other.md", "o\npeer edit\n")
	mustGit(t, f.repo, "pull", "-q", "--rebase", "origin", "main")
	mine := "h\ntoken " + fakeSecret() + "\n"
	f.editLocal("held.md", mine)

	f.loop.startCycle(f.repo, f.roots)
	if got, _ := f.claudeCopy("held.md"); got != mine {
		t.Errorf("held-back Claude copy overwritten: %q", redact(got))
	}
	if got, _ := f.claudeCopy("other.md"); got != "o\npeer edit\n" {
		t.Errorf("other.md = %q", got)
	}
}

// History rewritten by hand (a rebase): the diff between the record and HEAD is
// propagated for files whose Claude copy still equals the record's version; a
// file the user changed since keeps its Claude copy, and the incoming version
// is backed up.
func TestCatchUp_RewrittenHistoryOnlyTouchesUnchangedFiles(t *testing.T) {
	f := newPullFixture(t, map[string]string{"clean.md": "a\nb\n", "mine.md": "m\n", "gone.md": "g\n"})
	logs := captureLog(t)
	f.loop.pullTick(f.repo, f.roots)
	rec := f.syncedHead()

	// HEAD moves to a commit that does not descend from the record.
	mustGit(t, f.repo, "checkout", "-q", "--orphan", "rewritten")
	mustGit(t, f.repo, "rm", "-rq", "--cached", ".")
	writeRepoFile(t, f.repo, "projects/p1/memory/clean.md", "a\nrewritten\n")
	writeRepoFile(t, f.repo, "projects/p1/memory/mine.md", "m\nrewritten\n")
	if err := os.Remove(filepath.Join(f.roots.Mirror, "p1", "memory", "gone.md")); err != nil {
		t.Fatal(err)
	}
	mustGit(t, f.repo, "add", "-A")
	mustGit(t, f.repo, "commit", "-q", "-m", "rewritten")
	mustGit(t, f.repo, "branch", "-M", "main")
	if _, err := f.repo.Run("merge-base", "--is-ancestor", rec, "HEAD"); err == nil {
		t.Fatal("HEAD should not descend from the record")
	}
	writeMem(t, f.roots.Claude, "p1", "mine.md", "m\nuser edit\n") // changed since the record

	f.loop.startCycle(f.repo, f.roots)
	if got, _ := f.claudeCopy("clean.md"); got != "a\nrewritten\n" {
		t.Errorf("unchanged file not propagated: %q", got)
	}
	if got, _ := f.claudeCopy("mine.md"); got != "m\nuser edit\n" {
		t.Errorf("user's edit overwritten: %q", got)
	}
	if _, ok := f.claudeCopy("gone.md"); ok {
		t.Error("file deleted in the rewritten history still in Claude")
	}
	if b := f.backups("mine.md"); len(b) != 1 {
		t.Errorf("want one backup of the incoming mine.md, got %v", b)
	} else if got, _ := os.ReadFile(b[0]); string(got) != "m\nrewritten\n" {
		t.Errorf("backup = %q", got)
	}
	if !strings.Contains(logs.String(), "rewritten") {
		t.Errorf("want the rewrite logged:\n%s", logs)
	}
	if f.syncedHead() != f.head() {
		t.Errorf("record %q, HEAD %q", f.syncedHead(), f.head())
	}
}
