package sync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// unfinishedParkFixture is a crash after the park with one saved file lost, so
// the park cannot finish. The mirror holds the pre-edit MEMORY.md, with the
// "## Old" section the user has since removed from the Claude copy; a
// Reconcile now would resurrect it.
func unfinishedParkFixture(t *testing.T) *pullFixture {
	t.Helper()
	mem := "# Memory\n\n## Old\n- [O](o.md) — old\n\n## Keep\n- [K](k.md) — k\n"
	f := newPullFixture(t, map[string]string{"MEMORY.md": mem})
	manifest, err := BuildFromClaudeTree(f.roots.Claude)
	if err != nil {
		t.Fatal(err)
	}
	if err := manifest.Save(f.loop.Cfg.SyncDir); err != nil {
		t.Fatal(err)
	}
	memLocal := "# Memory\n\n## Keep\n- [K](k.md) — k token: " + fakeSecret() + "\n"
	f.editLocal("MEMORY.md", memLocal)
	if _, err := CommitMirror(f.repo, f.loop.Cfg, "c"); err != nil {
		t.Fatal(err)
	}
	if _, err := parkUncommitted(f.repo); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(parkBytesDir(f.repo), "projects", "p1", "memory", "MEMORY.md")); err != nil {
		t.Fatal(err)
	}
	return f
}

// Reconcile never runs over an unfinished park: at startup...
func TestPark_StartupSkipsReconcileWhileParkUnfinished(t *testing.T) {
	f := unfinishedParkFixture(t)
	logs := captureLog(t)
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
	if got, _ := f.claudeCopy("MEMORY.md"); strings.Contains(got, "## Old") {
		t.Errorf("Reconcile over an unfinished park resurrected a removed MEMORY.md section")
	}
	if !strings.Contains(logs.String(), "reconcile skipped") {
		t.Errorf("want the skip logged:\n%s", logs)
	}
	if !parkedOnDisk(f) {
		t.Error("the unfinished park should stay recorded")
	}
}

// ...and on the pull tick.
func TestPark_PullTickSkipsReconcileWhileParkUnfinished(t *testing.T) {
	f := unfinishedParkFixture(t)
	logs := captureLog(t)
	f.loop.pullTick(f.repo, f.roots)
	if got, _ := f.claudeCopy("MEMORY.md"); strings.Contains(got, "## Old") {
		t.Errorf("Reconcile over an unfinished park resurrected a removed MEMORY.md section")
	}
	if !strings.Contains(logs.String(), "reconcile skipped") {
		t.Errorf("want the skip logged:\n%s", logs)
	}
}

// Once the park is done Reconcile runs again.
func TestPark_PullTickReconcilesAfterParkFinishes(t *testing.T) {
	f := newPullFixture(t, map[string]string{"a.md": "a\n"})
	captureLog(t)
	f.peerPush("b.md", "peer note\n")
	if err := f.repo.Pull("main"); err != nil {
		t.Fatal(err)
	}
	f.loop.pullTick(f.repo, f.roots)
	if got, ok := f.claudeCopy("b.md"); !ok || got != "peer note\n" {
		t.Errorf("Reconcile did not run: %q, %v", got, ok)
	}
}

// A stuck MEMORY.md (3 failed restores) is set aside. The mirror copy then
// holds the HEAD version, so the Claude copy is copied over it: Reconcile must
// not merge a section the user removed back in.
func TestPark_StuckMemoryIndexNotResurrectedAfterSetAside(t *testing.T) {
	f := unfinishedParkFixture(t)
	captureLog(t)
	for i := 0; i < 5; i++ {
		f.loop.pullTick(f.repo, f.roots)
	}
	if parkedOnDisk(f) {
		t.Fatal("park still unfinished after 5 cycles")
	}
	if got, _ := f.claudeCopy("MEMORY.md"); strings.Contains(got, "## Old") {
		t.Errorf("removed MEMORY.md section resurrected in the Claude copy after set-aside")
	}
	if m, _ := os.ReadFile(filepath.Join(f.roots.Mirror, "p1", "memory", "MEMORY.md")); strings.Contains(string(m), "## Old") {
		t.Errorf("mirror copy still holds the removed section")
	}
}
