package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/MarimerLLC/claude-utils/internal/archive"
	"github.com/MarimerLLC/claude-utils/internal/changes"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

const archDay = "2026-10-25"

func archClock(t *testing.T) {
	t.Helper()
	old := archive.Now
	archive.Now = func() time.Time { return time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { archive.Now = old })
}

// archivePC makes a fixture with archiving on.
func archivePC(t *testing.T, seed map[string]string) *pullFixture {
	t.Helper()
	archClock(t)
	captureLog(t)
	f := newPullFixture(t, seed)
	f.loop.Cfg.ArchiveDeletes = true
	return f
}

// userDeletes is what the watcher sees when the user deletes a Claude note.
func (f *pullFixture) userDeletes(name string) {
	f.t.Helper()
	p := filepath.Join(f.roots.Claude, "p1", "memory", name)
	if err := os.Remove(p); err != nil {
		f.t.Fatal(err)
	}
	handled, err := f.loop.handleEvent(nil, f.roots, fsnotify.Event{Name: p, Op: fsnotify.Remove})
	if err != nil || !handled {
		f.t.Fatalf("remove event: handled=%v err=%v", handled, err)
	}
}

func (f *pullFixture) flushOK() {
	f.t.Helper()
	if err := f.loop.flush(f.repo, f.roots, true); err != nil {
		f.t.Fatal(err)
	}
}

func headTree(t *testing.T, r *gitwt.Repo) string {
	t.Helper()
	return mustGit(t, r, "ls-tree", "-r", "--name-only", "HEAD")
}

func TestArchive_DeleteOnHostA_ArchivedInRepo_RemovedOnHostB(t *testing.T) {
	a := archivePC(t, map[string]string{"gone.md": "remember this\n", "keep.md": "k\n"})
	b := a.addPC("pc-b")
	b.loop.Cfg.ArchiveDeletes = true
	arch := "archive/" + archDay + "/p1/gone.md"

	a.userDeletes("gone.md")
	a.flushOK()

	tree := headTree(t, a.repo)
	if strings.Contains(tree, "projects/p1/memory/gone.md") || !strings.Contains(tree, arch) {
		t.Fatalf("A's tree:\n%s", tree)
	}
	if got := mustGit(t, a.repo, "show", "HEAD:"+arch); got != "remember this\n" {
		t.Fatalf("archived bytes: %q", got)
	}
	idx := mustGit(t, a.repo, "show", "HEAD:archive/INDEX.md")
	for _, w := range []string{archDay, "p1", "gone.md", "deleted on a host"} {
		if !strings.Contains(idx, w) {
			t.Errorf("INDEX.md lacks %q:\n%s", w, idx)
		}
	}
	// One commit: the delete and the archive copy arrive together.
	if n := strings.TrimSpace(mustGit(t, a.repo, "rev-list", "--count", "HEAD~1..HEAD")); n != "1" {
		t.Errorf("commits for the delete: %s", n)
	}
	if st := mustGit(t, a.repo, "show", "--name-status", "-M", "--format=", "HEAD"); !strings.Contains(st, "R") {
		t.Errorf("git should see a move:\n%s", st)
	}

	if _, ok := b.claudeCopy("gone.md"); !ok {
		t.Fatal("setup: B should start with the note")
	}
	b.flushOK()
	if _, ok := b.claudeCopy("gone.md"); ok {
		t.Error("B still has the note in ~/.claude")
	}
	if _, ok := b.claudeCopy("keep.md"); !ok {
		t.Error("B lost an unrelated note")
	}
	if got, err := os.ReadFile(filepath.Join(b.repo.Dir, filepath.FromSlash(arch))); err != nil || string(got) != "remember this\n" {
		t.Errorf("B's repo copy of the archive: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(b.roots.Claude, "archive")); err == nil {
		t.Error("archive/ appeared in B's ~/.claude")
	}
	// Still in history, with the note's own path.
	if out := mustGit(t, a.repo, "log", "--all", "--format=%H", "--", "projects/p1/memory/gone.md"); strings.Count(out, "\n") < 2 {
		t.Errorf("history of the note:\n%s", out)
	}
}

func TestArchive_SameNameSameDay_GetsSuffix(t *testing.T) {
	a := archivePC(t, map[string]string{"gone.md": "first\n"})
	a.userDeletes("gone.md")
	a.flushOK()
	a.editLocal("gone.md", "second\n")
	a.flushOK()
	a.userDeletes("gone.md")
	a.flushOK()

	var found []string
	for _, l := range strings.Split(headTree(t, a.repo), "\n") {
		if strings.HasPrefix(l, "archive/"+archDay+"/p1/gone") {
			found = append(found, l)
		}
	}
	if len(found) != 2 {
		t.Fatalf("want 2 archived copies, got %v", found)
	}
	got := map[string]bool{}
	for _, p := range found {
		got[mustGit(t, a.repo, "show", "HEAD:"+p)] = true
	}
	if !got["first\n"] || !got["second\n"] {
		t.Errorf("an archived copy was overwritten: %v", got)
	}
	if n := len(strings.Split(strings.TrimSpace(mustGit(t, a.repo, "show", "HEAD:archive/INDEX.md")), "\n")); n != 2 {
		t.Errorf("INDEX.md lines mentioning the note: %d", n)
	}
}

func TestArchive_RenameStaysARename(t *testing.T) {
	body := strings.Repeat("a line long enough for rename detection\n", 6)
	a := archivePC(t, map[string]string{"old.md": body})
	oldP := filepath.Join(a.roots.Claude, "p1", "memory", "old.md")
	newP := filepath.Join(a.roots.Claude, "p1", "memory", "new.md")
	if err := os.Rename(oldP, newP); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []fsnotify.Event{{Name: oldP, Op: fsnotify.Rename}, {Name: newP, Op: fsnotify.Create}} {
		if _, err := a.loop.handleEvent(nil, a.roots, ev); err != nil {
			t.Fatal(err)
		}
	}
	a.flushOK()
	tree := headTree(t, a.repo)
	if strings.Contains(tree, "archive/") {
		t.Errorf("a rename was archived:\n%s", tree)
	}
	if !strings.Contains(tree, "projects/p1/memory/new.md") || strings.Contains(tree, "old.md") {
		t.Errorf("tree:\n%s", tree)
	}
}

func TestArchive_NeverPropagatedAndIgnoredByWalkers(t *testing.T) {
	a := archivePC(t, map[string]string{"keep.md": "k\n"})
	a.peerWrite("archive/"+archDay+"/p1/memory/old.md", "archived elsewhere\n")
	a.peerWrite("archive/"+archDay+"/p1/old.md", "archived elsewhere\n")
	a.flushOK()
	if _, err := os.Stat(filepath.Join(a.roots.Claude, "archive")); err == nil {
		t.Error("archive/ was mirrored into ~/.claude")
	}
	if _, ok := a.claudeCopy("old.md"); ok {
		t.Error("an archived note was copied back into a memory folder")
	}
	if _, err := os.Stat(filepath.Join(a.repo.Dir, "archive", archDay, "p1", "old.md")); err != nil {
		t.Fatalf("setup: archive file should be in the repo: %v", err)
	}

	// Reconcile: no archive folder in either tree afterwards, and no report line.
	m, err := BuildFromClaudeTree(a.roots.Claude)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"archive", "p1/memory/old.md"} {
		if m.Has(p) {
			t.Errorf("manifest has %s", p)
		}
	}
	rep, err := Reconcile(a.roots, m)
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(append(append(rep.CopiedToClaude, rep.CopiedToMirror...), rep.RemovedFromMirror...), " ")
	if strings.Contains(all, "old.md") || strings.Contains(all, "archive") {
		t.Errorf("reconcile touched the archive: %+v", rep)
	}
	dirs, _ := a.roots.MemoryDirs()
	for _, d := range dirs {
		if d == "archive" {
			t.Error("archive counted as a Claude folder")
		}
	}
	// changes lists only memory files, never archive entries.
	res, err := changes.Compute(changes.Options{SyncDir: a.repo.Dir, ClaudeDir: a.roots.Claude, Since: strings.TrimSpace(mustGit(t, a.repo, "rev-list", "--max-parents=0", "HEAD"))})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Text(), "archive") || strings.Contains(res.Text(), "old.md") {
		t.Errorf("changes lists archive entries:\n%s", res.Text())
	}
}

func TestArchive_FlagOff_DeleteIsADelete(t *testing.T) {
	a := archivePC(t, map[string]string{"gone.md": "bye\n"})
	a.loop.Cfg.ArchiveDeletes = false
	a.userDeletes("gone.md")
	a.flushOK()
	tree := headTree(t, a.repo)
	if strings.Contains(tree, "archive") || strings.Contains(tree, "gone.md") {
		t.Errorf("flag off must behave as before:\n%s", tree)
	}
}

func TestArchive_UnownedFolderIsNotArchivedByThisRepo(t *testing.T) {
	a := archivePC(t, map[string]string{"keep.md": "k\n"})
	a.roots.Owns = only("p1") // this repo does not own "pc"
	writeMem(t, a.roots.Claude, "pc", "client.md", "client note\n")
	// The client folder's note exists in this repo's history only as a frozen copy.
	a.peerWrite("projects/pc/memory/client.md", "client note\n")
	a.flushOK()
	pcNote := filepath.Join(a.roots.Claude, "pc", "memory", "client.md")
	if err := os.Remove(pcNote); err != nil {
		t.Fatal(err)
	}
	handled, err := a.loop.handleEvent(nil, a.roots, fsnotify.Event{Name: pcNote, Op: fsnotify.Remove})
	if err != nil || handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	a.flushOK()
	if tree := headTree(t, a.repo); strings.Contains(tree, "archive/") {
		t.Errorf("a folder this repo does not own was archived:\n%s", tree)
	}
}

// A copy the scrub holds back is not archived, and the deletion waits with it:
// the note must never leave the tree without its archive copy.
func TestArchive_HeldBackCopyKeepsTheDeletionBack(t *testing.T) {
	a := archivePC(t, map[string]string{"leak.md": "token: " + fakeSecret() + "\n"})
	a.userDeletes("leak.md")
	if _, err := CommitMirrorOwned(a.repo, a.loop.Cfg, "auto: test", nil); err != nil {
		t.Fatal(err)
	}
	tree := headTree(t, a.repo)
	if !strings.Contains(tree, "projects/p1/memory/leak.md") {
		t.Errorf("deletion committed without its archive copy:\n%s", tree)
	}
	if strings.Contains(tree, "archive/"+archDay+"/p1/leak.md") {
		t.Errorf("a flagged copy was committed:\n%s", tree)
	}
}

func TestArchive_MemoryIndexLineIsNotAnArchivedFile(t *testing.T) {
	a := archivePC(t, map[string]string{"MEMORY.md": "- [a](a.md) one\n- [b](b.md) two\n"})
	a.editLocal("MEMORY.md", "- [a](a.md) one\n")
	a.flushOK()
	if tree := headTree(t, a.repo); strings.Contains(tree, "archive/") {
		t.Errorf("an index line was archived as a file:\n%s", tree)
	}
}

func TestArchive_IndexMergesWithUnionDriver(t *testing.T) {
	a := archivePC(t, map[string]string{"one.md": "1\n", "two.md": "2\n"})
	b := a.addPC("pc-b")
	b.loop.Cfg.ArchiveDeletes = true
	a.userDeletes("one.md")
	a.flushOK()
	b.userDeletes("two.md")
	b.flushOK() // pulls A's INDEX.md line and adds its own
	idx := mustGit(t, b.repo, "show", "HEAD:archive/INDEX.md")
	if !strings.Contains(idx, "one.md") || !strings.Contains(idx, "two.md") {
		t.Errorf("INDEX.md lost a line:\n%s", idx)
	}
}
