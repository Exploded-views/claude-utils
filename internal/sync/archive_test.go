package sync

import (
	"crypto/sha256"
	"encoding/hex"
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

// archPath is where archive.Place puts a note's bytes on archDay.
func archPath(folder, name, content string) string {
	h := sha256.Sum256([]byte(content))
	ext := filepath.Ext(name)
	return "archive/" + archDay + "/" + folder + "/" + strings.TrimSuffix(name, ext) + "." + hex.EncodeToString(h[:])[:8] + ext
}

func headTree(t *testing.T, r *gitwt.Repo) string {
	t.Helper()
	return mustGit(t, r, "ls-tree", "-r", "--name-only", "HEAD")
}

func TestArchive_DeleteOnHostA_ArchivedInRepo_RemovedOnHostB(t *testing.T) {
	a := archivePC(t, map[string]string{"gone.md": "remember this\n", "keep.md": "k\n"})
	b := a.addPC("pc-b")
	b.loop.Cfg.ArchiveDeletes = true
	arch := archPath("p1", "gone.md", "remember this\n")

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

// The archive stays in the same repo, so it can only keep data: a delete is
// archived whoever owns the folder.
func TestArchive_AnyDeleteIsArchived(t *testing.T) {
	a := archivePC(t, map[string]string{"keep.md": "k\n"})
	a.roots.Owns = only("p1")
	a.peerWrite("projects/pc/memory/c.md", "frozen client copy\n")
	a.flushOK()
	if err := os.Remove(filepath.Join(a.roots.Mirror, "pc", "memory", "c.md")); err != nil {
		t.Fatal(err)
	}
	a.flushOK()
	tree := headTree(t, a.repo)
	if strings.Contains(tree, "projects/pc/memory/c.md") || !strings.Contains(tree, archPath("pc", "c.md", "frozen client copy\n")) {
		t.Errorf("delete not archived:\n%s", tree)
	}
}

// A copy the scrub holds back is not archived, and the deletion waits with it:
// the note must never leave the tree without its archive copy.
func TestArchive_HeldBackCopyKeepsTheDeletionBack(t *testing.T) {
	a := archivePC(t, map[string]string{"leak.md": "token: " + fakeSecret() + "\n"})
	a.userDeletes("leak.md")
	if _, err := CommitMirror(a.repo, a.loop.Cfg, "auto: test"); err != nil {
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

// A held-back copy must keep its note in the tree on every cycle, not only the
// first: the leftover copy on disk must not pair with the next delete as a
// rename. Neither the index line nor the union attribute is committed for it.
func TestArchive_HeldBackNoteSurvivesEveryCycle(t *testing.T) {
	a := archivePC(t, map[string]string{"leak.md": "token: " + fakeSecret() + "\n"})
	a.userDeletes("leak.md")
	for cycle := 1; cycle <= 3; cycle++ {
		if _, err := CommitMirror(a.repo, a.loop.Cfg, "auto: test"); err != nil {
			t.Fatal(err)
		}
		tree := headTree(t, a.repo)
		if !strings.Contains(tree, "projects/p1/memory/leak.md") {
			t.Fatalf("cycle %d: the note left the tree without its archive copy:\n%s", cycle, tree)
		}
		if strings.Contains(tree, "archive/") {
			t.Errorf("cycle %d: archive material committed for a held-back copy:\n%s", cycle, tree)
		}
		if strings.Contains(mustGit(t, a.repo, "show", "HEAD:.gitattributes"), "archive/INDEX.md") {
			t.Errorf("cycle %d: union attribute committed for a held-back copy", cycle)
		}
	}
	// And through the whole loop, twice (park, pull, restore in between).
	a.flushOK()
	a.flushOK()
	if tree := headTree(t, a.repo); !strings.Contains(tree, "projects/p1/memory/leak.md") || strings.Contains(tree, "archive/") {
		t.Errorf("after two flushes:\n%s", tree)
	}
}

// Delete plus a similar new note is git's R<100: the rename is kept, and the
// old content is archived too, so the edit loses nothing.
func TestArchive_NotQuiteARename_ArchivesTheOldSide(t *testing.T) {
	old := strings.Repeat("a line long enough for rename detection\n", 10)
	edited := old + "one more line\n"
	a := archivePC(t, map[string]string{"old.md": old})
	oldP := filepath.Join(a.roots.Claude, "p1", "memory", "old.md")
	newP := filepath.Join(a.roots.Claude, "p1", "memory", "new.md")
	_ = os.Remove(oldP)
	writeMem(t, a.roots.Claude, "p1", "new.md", edited)
	for _, ev := range []fsnotify.Event{{Name: oldP, Op: fsnotify.Remove}, {Name: newP, Op: fsnotify.Create}} {
		if _, err := a.loop.handleEvent(nil, a.roots, ev); err != nil {
			t.Fatal(err)
		}
	}
	a.flushOK()
	tree := headTree(t, a.repo)
	if !strings.Contains(tree, archPath("p1", "old.md", old)) {
		t.Errorf("old content not archived:\n%s", tree)
	}
	if !strings.Contains(tree, "projects/p1/memory/new.md") || strings.Contains(tree, "projects/p1/memory/old.md") {
		t.Errorf("tree:\n%s", tree)
	}
}

// Two hosts archive the same note on the same day with different bytes (one had
// not pulled the other's edit). Both pushes succeed and both versions are kept.
func TestArchive_TwoHostsSameDay_BothVersionsKept(t *testing.T) {
	a := archivePC(t, map[string]string{"x.md": "v0\n"})
	b := a.addPC("pc-b")
	b.loop.Cfg.ArchiveDeletes = true

	a.editLocal("x.md", "v1\n")
	a.flushOK()
	a.userDeletes("x.md")
	a.flushOK() // A archives v1 and pushes
	b.userDeletes("x.md")
	b.flushOK() // B (stale) archives v0, pulls and pushes

	for _, f := range []*pullFixture{a, b} {
		f.flushOK()
	}
	out := mustGit(t, gitwt.New(a.origin), "ls-tree", "-r", "--name-only", "main")
	for _, want := range []string{archPath("p1", "x.md", "v1\n"), archPath("p1", "x.md", "v0\n")} {
		if !strings.Contains(out, want) {
			t.Errorf("origin lacks %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "projects/p1/memory/x.md") {
		t.Errorf("note still live:\n%s", out)
	}
	if mustGit(t, a.repo, "rev-parse", "HEAD") != mustGit(t, gitwt.New(a.origin), "rev-parse", "main") {
		t.Error("A is not in step with origin")
	}
	idx := mustGit(t, b.repo, "show", "HEAD:archive/INDEX.md")
	if strings.Count(idx, "\n") != 2 {
		t.Errorf("INDEX.md:\n%s", idx)
	}
}

// Both hosts archive the identical note: one identical add, no conflict.
func TestArchive_TwoHostsSameBytes_Collapse(t *testing.T) {
	a := archivePC(t, map[string]string{"x.md": "same\n"})
	b := a.addPC("pc-b")
	b.loop.Cfg.ArchiveDeletes = true
	a.userDeletes("x.md")
	a.flushOK()
	b.userDeletes("x.md")
	b.flushOK()
	n := 0
	for _, l := range strings.Split(mustGit(t, gitwt.New(a.origin), "ls-tree", "-r", "--name-only", "main"), "\n") {
		if strings.HasPrefix(l, "archive/"+archDay+"/p1/x.") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("want one archived copy, got %d", n)
	}
}
