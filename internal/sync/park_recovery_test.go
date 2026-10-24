package sync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// distilledRel is a non-memory path: it has no Claude-side twin to refill from.
const distilledRel = "distilled/entry.md"

func (f *pullFixture) writeWork(rel, content string) {
	f.t.Helper()
	writeRepoFile(f.t, f.repo, rel, content)
}

func (f *pullFixture) workFile(rel string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(f.repo.Dir, filepath.FromSlash(rel)))
	return string(b), err == nil
}

func parkedExists(f *pullFixture) bool {
	_, err := os.Stat(parkStatePath(f.repo))
	return err == nil
}

// A crash after the pull but before the unpark leaves the pulled tree with
// the held-back files reset to HEAD. The next start finishes the unpark before
// Reconcile: no spurious .from-remote backup of a file the pull did not touch,
// and a MEMORY.md section the user removed does not come back.
func TestPark_CrashAfterPullBeforeUnpark(t *testing.T) {
	mem := "# Memory\n\n## Old\n- [O](o.md) — old\n\n## Keep\n- [K](k.md) — k\n"
	f := newPullFixture(t, map[string]string{"note.md": "alpha\n", "MEMORY.md": mem})
	captureLog(t)
	manifest, err := BuildFromClaudeTree(f.roots.Claude)
	if err != nil {
		t.Fatal(err)
	}
	noteLocal := "alpha token: " + fakeSecret() + "\n"
	memLocal := "# Memory\n\n## Keep\n- [K](k.md) — k token: " + fakeSecret() + "\n"
	f.editLocal("note.md", noteLocal)
	f.editLocal("MEMORY.md", memLocal)
	f.peerPush("peer.md", "p\n")

	if _, err := CommitMirror(f.repo, f.loop.Cfg, "c"); err != nil {
		t.Fatal(err)
	}
	if _, err := parkUncommitted(f.repo); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.Pull("main"); err != nil {
		t.Fatal(err)
	}
	// crash; restart: Run finishes the park first, then Reconcile and flush.
	if err := finishParked(f.repo, f.roots); err != nil {
		t.Fatal(err)
	}
	if _, err := Reconcile(f.roots, manifest); err != nil {
		t.Fatal(err)
	}
	if err := f.loop.flush(f.repo, f.roots, true); err != nil {
		t.Logf("flush: %v", err)
	}
	f.requirePreserved("note.md", noteLocal, nil)
	if n := len(f.backups("note.md")); n != 0 {
		t.Errorf("spurious .from-remote backups of note.md after crash recovery: %d", n)
	}
	if got, _ := f.claudeCopy("MEMORY.md"); strings.Contains(got, "## Old") {
		t.Errorf("crash recovery resurrected a section the user removed from MEMORY.md")
	}
	if parkedExists(f) {
		t.Error("parked state left behind")
	}
}

// The same crash through the daemon: Run must finish a leftover parked.json
// before its startup Reconcile.
func TestPark_RunFinishesLeftoverBeforeReconcile(t *testing.T) {
	mem := "# Memory\n\n## Old\n- [O](o.md) — old\n\n## Keep\n- [K](k.md) — k\n"
	f := newPullFixture(t, map[string]string{"note.md": "alpha\n", "MEMORY.md": mem})
	captureLog(t)
	manifest, err := BuildFromClaudeTree(f.roots.Claude)
	if err != nil {
		t.Fatal(err)
	}
	if err := manifest.Save(f.loop.Cfg.SyncDir); err != nil {
		t.Fatal(err)
	}
	memLocal := "# Memory\n\n## Keep\n- [K](k.md) — k token: " + fakeSecret() + "\n"
	f.editLocal("MEMORY.md", memLocal)
	f.editLocal("note.md", "alpha token: "+fakeSecret()+"\n")
	f.peerPush("peer.md", "p\n")
	if _, err := CommitMirror(f.repo, f.loop.Cfg, "c"); err != nil {
		t.Fatal(err)
	}
	if _, err := parkUncommitted(f.repo); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.Pull("main"); err != nil {
		t.Fatal(err)
	}

	flushed := make(chan error, 4)
	f.loop.OnFlush = func(_ bool, err error) { flushed <- err }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.loop.Run(ctx) }()
	select {
	case <-flushed:
	case <-time.After(10 * time.Second):
		t.Fatal("no startup flush")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := len(f.backups("note.md")) + len(f.backups("MEMORY.md")); n != 0 {
		t.Errorf("spurious backups after restart: %d", n)
	}
	if got, _ := f.claudeCopy("MEMORY.md"); strings.Contains(got, "## Old") {
		t.Errorf("restart resurrected a section the user removed from MEMORY.md")
	}
	if parkedExists(f) {
		t.Error("parked state left behind")
	}
}

// A crash between writing parked.json and resetting the work-tree: the saved
// bytes and the half-touched tree both restore to what the user had.
func TestPark_CrashBetweenStateAndReset(t *testing.T) {
	f := newPullFixture(t, map[string]string{"note.md": "alpha\n", "other.md": "o\n"})
	captureLog(t)
	noteLocal := "alpha token: " + fakeSecret() + "\n"
	entryLocal := "entry " + fakeSecret() + "\n"
	f.editLocal("note.md", noteLocal)
	f.writeWork(distilledRel, entryLocal)
	f.writeWork("remotes/x/y.md", "other layout\n")

	st, err := savePark(f.repo)
	if err != nil {
		t.Fatal(err)
	}
	if st == nil || len(st.Paths) != 3 {
		t.Fatalf("want 3 parked paths, got %+v", st)
	}
	if !parkedExists(f) {
		t.Fatal("savePark must write parked.json before the tree is touched")
	}
	// Crash with the tree only partly reset: one file is already gone.
	if err := os.Remove(filepath.Join(f.repo.Dir, filepath.FromSlash(distilledRel))); err != nil {
		t.Fatal(err)
	}
	mustGit(t, f.repo, "checkout", "-q", "HEAD", "--", "projects/p1/memory/note.md")

	if err := finishParked(f.repo, f.roots); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.workFile(distilledRel); got != entryLocal {
		t.Errorf("distilled entry not restored: %q", redact(got))
	}
	if got, _ := f.workFile("remotes/x/y.md"); got != "other layout\n" {
		t.Errorf("remotes file not restored: %q", got)
	}
	if m, _ := os.ReadFile(filepath.Join(f.roots.Mirror, "p1", "memory", "note.md")); string(m) != noteLocal {
		t.Errorf("mirror note not refilled from the Claude copy")
	}
	if parkedExists(f) {
		t.Error("parked state left behind")
	}
}

// Deletions are parked too: a tracked file the user deleted stays deleted
// after the park/pull/unpark round trip.
func TestPark_RecordsDeletions(t *testing.T) {
	f := newPullFixture(t, map[string]string{"note.md": "alpha\n"})
	captureLog(t)
	f.writeWork(distilledRel, "e\n")
	mustGit(t, f.repo, "add", "-A")
	mustGit(t, f.repo, "commit", "-q", "-m", "add entry")
	if err := os.Remove(filepath.Join(f.repo.Dir, filepath.FromSlash(distilledRel))); err != nil {
		t.Fatal(err)
	}
	st, err := parkUncommitted(f.repo)
	if err != nil {
		t.Fatal(err)
	}
	if st == nil || len(st.Paths) != 1 || st.Paths[0].Kind != parkDeleted {
		t.Fatalf("want one parked deletion, got %+v", st)
	}
	if _, ok := f.workFile(distilledRel); !ok {
		t.Fatal("the tree should be reset to HEAD while parked")
	}
	if err := finishParked(f.repo, f.roots); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.workFile(distilledRel); ok {
		t.Error("the deletion was not restored")
	}
}

// propagateChanges failing mid-cycle must still restore every parked file.
func TestPark_PropagateErrorStillUnparks(t *testing.T) {
	f := newPullFixture(t, map[string]string{"note.md": "alpha\nbeta\n"})
	captureLog(t)
	local := "alpha\nbeta token: " + fakeSecret() + "\n"
	entryLocal := "entry " + fakeSecret() + "\n"
	f.editLocal("note.md", local)
	f.writeWork(distilledRel, entryLocal)
	f.peerPush("note.md", "alpha\nbeta edited on peer\n")
	f.peerPush("peer.md", "p\n")

	old := propagate
	propagate = func(*gitwt.Repo, Roots, string, string, map[string]bool, *pathGuard) error { return errors.New("boom") }
	t.Cleanup(func() { propagate = old })

	err := f.loop.flush(f.repo, f.roots, true)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want the propagate error, got %v", err)
	}
	f.requirePreserved("note.md", local, err)
	if m, _ := os.ReadFile(filepath.Join(f.roots.Mirror, "p1", "memory", "note.md")); string(m) != local {
		t.Errorf("mirror copy not restored after the error")
	}
	if got, _ := f.workFile(distilledRel); got != entryLocal {
		t.Errorf("distilled entry not restored after the error: %q", redact(got))
	}
	if parkedExists(f) {
		t.Error("parked state left behind")
	}
	if st, _ := f.repo.Run("stash", "list"); st != "" {
		t.Errorf("stash should be empty, got %q", st)
	}
}

// Unpark tries every path: one that cannot be restored does not strand the
// rest, and it stays recorded so the next run retries it.
func TestPark_UnparkTriesEveryPath(t *testing.T) {
	f := newPullFixture(t, map[string]string{"note.md": "alpha\n"})
	captureLog(t)
	local := "alpha token: " + fakeSecret() + "\n"
	f.editLocal("note.md", local)
	f.writeWork("aaa/first.md", "first\n")
	f.writeWork("zzz/last.md", "last\n")
	if _, err := parkUncommitted(f.repo); err != nil {
		t.Fatal(err)
	}
	// Block the restore of aaa/first.md: its directory is now a file.
	if err := os.RemoveAll(filepath.Join(f.repo.Dir, "aaa")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.repo.Dir, "aaa"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := finishParked(f.repo, f.roots); err == nil {
		t.Fatal("want an error for the blocked path")
	}
	if got, _ := f.workFile("zzz/last.md"); got != "last\n" {
		t.Errorf("later path not restored after an earlier failure: %q", got)
	}
	if m, _ := os.ReadFile(filepath.Join(f.roots.Mirror, "p1", "memory", "note.md")); string(m) != local {
		t.Errorf("memory path not restored after an earlier failure")
	}
	st, err := loadPark(f.repo)
	if err != nil || st == nil || len(st.Paths) != 1 || st.Paths[0].Path != "aaa/first.md" {
		t.Fatalf("only the failed path should stay recorded, got %+v, %v", st, err)
	}

	// The next flush will not park over unrestored files.
	if err := f.loop.flush(f.repo, f.roots, true); err == nil {
		t.Error("flush should refuse to park while an earlier park is unfinished")
	}
	// Fix the cause; the retry finishes and clears the state.
	if err := os.Remove(filepath.Join(f.repo.Dir, "aaa")); err != nil {
		t.Fatal(err)
	}
	if err := finishParked(f.repo, f.roots); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.workFile("aaa/first.md"); got != "first\n" {
		t.Errorf("retry did not restore: %q", got)
	}
	if parkedExists(f) {
		t.Error("parked state left behind")
	}
}

// propagateChanges skips a parked path by its sync-repo path, whatever that
// path would map to in the Claude tree.
func TestPropagate_SkipsParkedRepoPath(t *testing.T) {
	f := newPullFixture(t, map[string]string{"a.md": "a\n", "b.md": "b\n"})
	captureLog(t)
	pre := strings.TrimSpace(mustGit(t, f.repo, "rev-parse", "HEAD"))
	f.peerPush("a.md", "a2\n")
	f.peerPush("b.md", "b2\n")
	if err := f.repo.Pull("main"); err != nil {
		t.Fatal(err)
	}
	post := strings.TrimSpace(mustGit(t, f.repo, "rev-parse", "HEAD"))
	if err := propagateChanges(f.repo, f.roots, pre, post, map[string]bool{"projects/p1/memory/a.md": true}, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.claudeCopy("a.md"); got != "a\n" {
		t.Errorf("parked path was propagated: %q", got)
	}
	if got, _ := f.claudeCopy("b.md"); got != "b2\n" {
		t.Errorf("unparked path not propagated: %q", got)
	}
}
