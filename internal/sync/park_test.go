package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// peerRename renames a memory file on the peer and pushes it.
func (f *pullFixture) peerRename(from, to string) {
	f.t.Helper()
	mustGit(f.t, f.peer, "pull", "-q", "origin", "main")
	dir := "projects/p1/memory/"
	mustGit(f.t, f.peer, "mv", dir+from, dir+to)
	mustGit(f.t, f.peer, "commit", "-q", "-m", "peer: rename "+from)
	mustGit(f.t, f.peer, "push", "-q", "origin", "main")
}

func (f *pullFixture) backups(name string) []string {
	m, _ := filepath.Glob(filepath.Join(f.roots.Mirror, "p1", "memory", name+".from-remote-*"))
	return m
}

// A held-back MEMORY.md that the peer also changed is merged with the
// peer's version, not shelved as a backup: the Claude copy ends up with both
// sides' sections and the mirror copy matches it.
func TestPark_MemoryIndexMergedNotBackedUp(t *testing.T) {
	base := "# Memory\n\n## Shared\n- [A](a.md) — a\n"
	f := newPullFixture(t, map[string]string{"MEMORY.md": base})
	captureLog(t)
	local := base + "\n## Local\n- token: \"" + strings.Repeat("q", 20) + "\"\n"
	f.editLocal("MEMORY.md", local)
	f.peerPush("MEMORY.md", base+"\n## Peer\n- [P](p.md) — p\n")

	err := f.loop.flush(f.repo, f.roots, true)
	f.requirePreserved("MEMORY.md", local, err)

	got, _ := f.claudeCopy("MEMORY.md")
	if !strings.Contains(got, "## Peer") || !strings.Contains(got, "## Local") {
		t.Errorf("Claude copy should hold both sections, got:\n%s", redact(got))
	}
	if b := f.backups("MEMORY.md"); len(b) != 0 {
		t.Errorf("merge succeeded, want no backup, got %v", b)
	}
	if m, _ := os.ReadFile(filepath.Join(f.roots.Mirror, "p1", "memory", "MEMORY.md")); string(m) != got {
		t.Errorf("mirror copy should match the merged Claude copy")
	}
}

// When the merge conflicts the Claude copy is left alone and the incoming
// version is kept as a backup.
func TestPark_MemoryIndexConflictFallsBackToBackup(t *testing.T) {
	base := "# Memory\n\n## Notes\nsome prose line\n"
	f := newPullFixture(t, map[string]string{"MEMORY.md": base})
	captureLog(t)
	local := "# Memory\n\n## Notes\nsome prose line token: " + fakeSecret() + "\n"
	peer := "# Memory\n\n## Notes\nsome prose line, edited on peer\n"
	f.editLocal("MEMORY.md", local)
	f.peerPush("MEMORY.md", peer)

	err := f.loop.flush(f.repo, f.roots, true)
	f.requirePreserved("MEMORY.md", local, err)

	if got, _ := f.claudeCopy("MEMORY.md"); got != local {
		t.Errorf("conflicting merge must leave the Claude copy alone, got:\n%s", redact(got))
	}
	b := f.backups("MEMORY.md")
	if len(b) != 1 {
		t.Fatalf("want one backup, got %v", b)
	}
	if c, _ := os.ReadFile(b[0]); string(c) != peer {
		t.Errorf("backup should hold the peer's version, got %q", c)
	}
}

// The peer renames a file that is held back here. The old path is parked, so
// the rename must not delete the Claude copy.
func TestPark_RenamedUpstreamKeepsHeldBackOldPath(t *testing.T) {
	body := strings.Repeat("a line long enough for rename detection\n", 8)
	f := newPullFixture(t, map[string]string{"old.md": body})
	captureLog(t)
	local := body + "token: " + fakeSecret() + "\n"
	f.editLocal("old.md", local)
	f.peerRename("old.md", "new.md")

	err := f.loop.flush(f.repo, f.roots, true)
	f.requirePreserved("old.md", local, err)
	if got, ok := f.claudeCopy("new.md"); !ok || got != body {
		t.Errorf("the renamed file should still arrive, got ok=%v", ok)
	}
}

// A crash between park and refill leaves the mirror without the user's
// version. The Claude copy was never touched, so the startup Reconcile plus
// the next flush simply re-mirror it: nothing is lost and nothing needs a
// separate recovery path.
func TestPark_CrashBeforeRefillRecovers(t *testing.T) {
	f := newPullFixture(t, map[string]string{"note.md": "alpha\nbeta\n", "MEMORY.md": "# Memory\n"})
	captureLog(t)
	noteLocal := "alpha\nbeta token: " + fakeSecret() + "\n"
	newLocal := "never committed " + fakeSecret() + "\n"
	f.editLocal("note.md", noteLocal)
	f.editLocal("new.md", newLocal)

	if _, err := parkUncommitted(f.repo); err != nil {
		t.Fatal(err)
	}
	// "Crash": no unpark. The mirror lost the local versions...
	if m, _ := os.ReadFile(filepath.Join(f.roots.Mirror, "p1", "memory", "note.md")); string(m) == noteLocal {
		t.Fatal("park should have reset the mirror copy")
	}
	// ...but the Claude copies are intact, which is all recovery needs.
	for name, want := range map[string]string{"note.md": noteLocal, "new.md": newLocal} {
		if got, _ := f.claudeCopy(name); got != want {
			t.Fatalf("Claude copy of %s changed by park", name)
		}
	}

	// Restart: Reconcile, then flush.
	if _, err := Reconcile(f.roots, NewManifest()); err != nil {
		t.Fatal(err)
	}
	if err := f.loop.flush(f.repo, f.roots, true); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"note.md": noteLocal, "new.md": newLocal} {
		if got, _ := f.claudeCopy(name); got != want {
			t.Errorf("Claude copy of %s changed by recovery:\n%s", name, redact(got))
		}
		if m, _ := os.ReadFile(filepath.Join(f.roots.Mirror, "p1", "memory", name)); string(m) != want {
			t.Errorf("mirror copy of %s was not re-mirrored", name)
		}
	}
}
