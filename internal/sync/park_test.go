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

// A crash between park and unpark leaves the mirror without the user's
// version, and parked.json on disk. The next start finishes the park, then
// Reconcile and the next flush carry on; the Claude copies were never touched.
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

	// Restart: finish the park, Reconcile, then flush.
	if err := finishParked(f.repo, f.roots); err != nil {
		t.Fatal(err)
	}
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

// Liveness: a tracked held-back file edited on the peer must not
// stop unrelated notes flowing both ways, over two cycles.
func TestPark_LivenessTrackedHeldBackEditedUpstream(t *testing.T) {
	f := newPullFixture(t, map[string]string{"note.md": "alpha\nbeta\n"})
	captureLog(t)
	local := "alpha\nbeta token: " + fakeSecret() + "\n"
	f.editLocal("note.md", local)
	f.editLocal("mine1.md", "mine one\n")
	f.peerPush("note.md", "alpha\nbeta edited on peer\n")
	f.peerPush("peer1.md", "peer one\n")

	err := f.loop.flush(f.repo, f.roots, true)
	f.requirePreserved("note.md", local, err)
	if !f.originHas("projects/p1/memory/mine1.md") {
		t.Errorf("cycle 1: mine1.md not pushed (err %v)", err)
	}
	if _, ok := f.claudeCopy("peer1.md"); !ok {
		t.Errorf("cycle 1: peer1.md not pulled into Claude")
	}

	// Cycle 2: the peer edits the held-back file again plus another note.
	f.peerPush("note.md", "alpha\nbeta edited on peer twice\n")
	f.peerPush("peer2.md", "peer two\n")
	f.editLocal("mine2.md", "mine two\n")
	err = f.loop.flush(f.repo, f.roots, true)
	f.requirePreserved("note.md", local, err)
	if !f.originHas("projects/p1/memory/mine2.md") {
		t.Errorf("cycle 2: mine2.md not pushed (err %v)", err)
	}
	if _, ok := f.claudeCopy("peer2.md"); !ok {
		t.Errorf("cycle 2: peer2.md not pulled into Claude")
	}
	if st, _ := f.repo.Run("stash", "list"); st != "" {
		t.Errorf("stash should be empty, got %q", st)
	}
}

// A parked path need not be a memory file. An untracked held-back file outside
// projects/ while the peer adds the same path must not stop the pull: git
// would refuse to overwrite it, so every cycle would fail and nothing
// unrelated would be pulled or pushed.
func TestPark_DistilledUntrackedHeldBackDoesNotStall(t *testing.T) {
	f := newPullFixture(t, map[string]string{"note.md": "n\n"})
	captureLog(t)
	entry := filepath.Join(f.repo.Dir, "distilled", "entry.md")
	if err := os.MkdirAll(filepath.Dir(entry), 0700); err != nil {
		t.Fatal(err)
	}
	local := "mine token: " + fakeSecret() + "\n"
	if err := os.WriteFile(entry, []byte(local), 0600); err != nil {
		t.Fatal(err)
	}
	f.peerWrite("distilled/entry.md", "theirs\n")
	f.peerPush("peernote.md", "peer note\n")
	f.editLocal("mine.md", "mine\n")

	var err error
	for i := 0; i < 2; i++ {
		err = f.loop.flush(f.repo, f.roots, true)
	}
	if err != nil {
		t.Errorf("last flush err: %v", err)
	}
	if b, _ := os.ReadFile(entry); string(b) != local {
		t.Errorf("distilled entry changed: %q", redact(string(b)))
	}
	if _, ok := f.claudeCopy("peernote.md"); !ok {
		t.Errorf("peernote.md never pulled while distilled/entry.md is held back")
	}
	if !f.originHas("projects/p1/memory/mine.md") {
		t.Errorf("mine.md never pushed while distilled/entry.md is held back")
	}
}

// A tracked held-back distilled entry that the peer edits on the same line.
// distilled/ has no Claude-side twin, so the work-tree file is the only copy of
// the user's version: it must come back intact (no conflict markers, no
// autostash entry) with the incoming version kept as a .from-remote-* backup.
func TestPark_DistilledTrackedHeldBackKeepsLocalAndBacksUpIncoming(t *testing.T) {
	f := newPullFixture(t, map[string]string{"note.md": "n\n"})
	captureLog(t)
	f.peerWrite("distilled/entry.md", "alpha\nbeta\n")
	if err := f.loop.flush(f.repo, f.roots, true); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(f.repo.Dir, "distilled", "entry.md")
	local := "alpha\nbeta token: " + fakeSecret() + "\n"
	if err := os.WriteFile(entry, []byte(local), 0600); err != nil {
		t.Fatal(err)
	}
	f.peerWrite("distilled/entry.md", "alpha\nbeta edited on peer\n")

	err := f.loop.flush(f.repo, f.roots, true)
	b, _ := os.ReadFile(entry)
	st, _ := f.repo.Run("stash", "list")
	if err != nil || st != "" {
		t.Errorf("flush err: %v; stash: %q", err, st)
	}
	if string(b) != local {
		t.Errorf("distilled entry (only copy) damaged:\n%s", redact(string(b)))
	}
	bk, _ := filepath.Glob(entry + ".from-remote-*")
	if len(bk) != 1 {
		t.Fatalf("want one backup of the incoming entry, got %v", bk)
	}
	if c, _ := os.ReadFile(bk[0]); string(c) != "alpha\nbeta edited on peer\n" {
		t.Errorf("backup = %q", c)
	}
}
