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

var bigNote = strings.Repeat("a long line, so git sees the copy as a rename\n", 10)

// The same two-host case with notes big enough for git to see each archive copy
// as a rename of the note: the two renames collide (rename/rename, one note to
// two archive paths), and the pull must resolve it and keep both versions.
func TestArchive_TwoHostsSameDay_RenameRenameResolved(t *testing.T) {
	a := archivePC(t, map[string]string{"x.md": bigNote + "v0\n"})
	b := a.addPC("pc-b")
	b.loop.Cfg.ArchiveDeletes = true
	logs := captureLog(t)

	a.editLocal("x.md", bigNote+"v1\n")
	a.flushOK()
	a.userDeletes("x.md")
	a.flushOK()
	b.userDeletes("x.md")
	b.flushOK()
	b.flushOK()

	out := mustGit(t, gitwt.New(a.origin), "ls-tree", "-r", "--name-only", "main")
	for _, v := range []string{"v0", "v1"} {
		if !strings.Contains(out, archPath("p1", "x.md", bigNote+v+"\n")) {
			t.Errorf("origin lacks the %s copy:\n%s", v, out)
		}
	}
	if strings.Contains(out, "projects/p1/memory/x.md") {
		t.Errorf("note still live:\n%s", out)
	}
	if b.repo.RebaseInProgress() || b.repo.HasUnmerged() {
		t.Error("B is wedged in a rebase")
	}
	if !strings.Contains(logs.String(), "auto-resolved") {
		t.Errorf("no count-only log line:\n%s", logs.String())
	}
	if _, ok := b.claudeCopy("x.md"); ok {
		t.Error("B's ~/.claude still has the note")
	}
}

// A peer's real rename and a local edit of the old name merge as before (the
// default merge follows the rename), with archiving on.
func TestArchive_PeerRenameAndLocalEditCarriedAcross(t *testing.T) {
	a := archivePC(t, map[string]string{"old.md": bigNote})
	a.editLocal("old.md", bigNote+"local edit\n")
	a.peerRename("old.md", "new.md")
	a.flushOK()
	got, ok := a.claudeCopy("new.md")
	if !ok || !strings.Contains(got, "local edit") {
		t.Fatalf("new.md = %q ok=%v", got, ok)
	}
	if _, ok := a.claudeCopy("old.md"); ok {
		t.Error("old.md survived the rename")
	}
	if tree := headTree(t, a.repo); strings.Contains(tree, "archive/") {
		t.Errorf("a rename was archived:\n%s", tree)
	}
}

// A conflict that mixes an archive path with an ordinary note conflict is not
// in the safe set: the pull aborts as it always did.
func TestArchive_MixedConflictAborts(t *testing.T) {
	a := archivePC(t, map[string]string{"x.md": bigNote + "v0\n", "z.md": "one\ntwo\nthree\n"})
	b := a.addPC("pc-b")
	b.loop.Cfg.ArchiveDeletes = true
	a.editLocal("x.md", bigNote+"v1\n")
	a.editLocal("z.md", "one\nA's line\nthree\n")
	a.flushOK()
	a.userDeletes("x.md")
	a.flushOK()
	b.userDeletes("x.md")
	b.editLocal("z.md", "one\nB's line\nthree\n")
	err := b.loop.flush(b.repo, b.roots, true)
	if err == nil || !strings.Contains(err.Error(), "rebase conflict") {
		t.Fatalf("want the usual conflict abort, got %v", err)
	}
	if b.repo.RebaseInProgress() {
		t.Error("the rebase was left stopped")
	}
	if b.originHas("archive/") && strings.Contains(mustGit(t, gitwt.New(b.origin), "ls-tree", "-r", "--name-only", "main"), ".v0") {
		t.Error("B's commit was pushed")
	}
}

// Two hosts adding the same ordinary note differently still abort.
func TestArchive_PlainAddAddAborts(t *testing.T) {
	a := archivePC(t, map[string]string{"keep.md": "k\n"})
	a.peerPush("n.md", "peer's\n")
	a.editLocal("n.md", "mine\n")
	err := a.loop.flush(a.repo, a.roots, true)
	if err == nil || !strings.Contains(err.Error(), "rebase conflict") || a.repo.RebaseInProgress() {
		t.Fatalf("got %v, in progress %v", err, a.repo.RebaseInProgress())
	}
}

func TestPlanArchiveResolution_SafeSetIsExact(t *testing.T) {
	u := func(p string, st int, oid string) gitwt.UnmergedEntry {
		return gitwt.UnmergedEntry{Path: p, Stage: st, Oid: oid}
	}
	arch := "archive/2026-10-25/p1/x.aaaaaaaa.md"
	note := "projects/p1/memory/x.md"
	safe := [][]gitwt.UnmergedEntry{
		{u(arch, 1, "b"), u(arch, 3, "c")}, // DU
		{u(arch, 3, "c")},                  // UA
		{u(arch, 2, "c")},                  // AU
		{u(note, 1, "b"), u(arch, 2, "c"), u("archive/2026-10-25/p1/x.bbbbbbbb.md", 3, "d")}, // rename/rename
		{u(arch, 2, "c"), u(arch, 3, "c")},                                                   // identical add/add
	}
	for i, es := range safe {
		if _, ok := planArchiveResolution(es); !ok {
			t.Errorf("safe shape %d refused", i)
		}
	}
	unsafe := [][]gitwt.UnmergedEntry{
		nil,
		{u("archive/INDEX.md", 1, "a"), u("archive/INDEX.md", 2, "b"), u("archive/INDEX.md", 3, "c")},
		{u(arch, 1, "b"), u(arch, 2, "c")},                  // the commit deleted a copy
		{u(arch, 2, "c"), u(arch, 3, "d")},                  // differing add/add
		{u(note, 1, "a"), u(note, 2, "b"), u(note, 3, "c")}, // a note's content conflict
		{u(note, 2, "b"), u(note, 3, "c")},                  // add/add of a note
		{u(note, 1, "a"), u(note, 2, "b")},                  // modify/delete of a note
		{u(arch, 3, "c"), u(note, 2, "b"), u(note, 3, "c")}, // one safe path, one not
		{u("distilled/e.md", 1, "a")},
	}
	for i, es := range unsafe {
		if _, ok := planArchiveResolution(es); ok {
			t.Errorf("unsafe shape %d accepted", i)
		}
	}
}

// allArchiveCopiesMatchTheirNames is the invariant: every hash-named copy in
// the tree holds bytes that hash to its name.
func allArchiveCopiesMatchTheirNames(t *testing.T, r *gitwt.Repo) {
	t.Helper()
	for _, l := range strings.Split(strings.TrimSpace(headTree(t, r)), "\n") {
		parts := strings.Split(l, "/")
		if len(parts) != 4 || parts[0] != "archive" || parts[2] == archive.DistilledFolder {
			continue
		}
		body := mustGit(t, r, "show", "HEAD:"+l)
		if _, _, ok := archiveNameHash(parts[3]); !ok {
			continue
		}
		if !archiveBytesMatch(parts[3], []byte(body)) {
			t.Errorf("%s no longer matches its hash name", l)
		}
	}
}

func claudeHas(t *testing.T, f *pullFixture, name, want string) {
	t.Helper()
	if got, ok := f.claudeCopy(name); !ok || got != want {
		t.Errorf("%s: ~/.claude has %q ok=%v, want %q", name, got, ok, want)
	}
}

// The peer archives a note while this host edits it. The edit merges into the
// archive copy (a 100% rename), which would hide the edit and break the copy's
// name: both must survive, the edit live and the old text archived.
func TestArchive_EditHere_ArchiveOnPeer_BothKept(t *testing.T) {
	v0, v1 := bigNote+"v0\n", bigNote+"v1 edited\n"
	a := archivePC(t, map[string]string{"x.md": v0})
	b := a.addPC("pc-b")
	b.loop.Cfg.ArchiveDeletes = true
	logs := captureLog(t)

	a.userDeletes("x.md")
	a.flushOK() // peer archives and pushes
	b.editLocal("x.md", v1)
	b.flushOK() // this host pulls with an edit in flight
	a.flushOK()

	for _, f := range []*pullFixture{a, b} {
		claudeHas(t, f, "x.md", v1)
		allArchiveCopiesMatchTheirNames(t, f.repo)
		if tree := headTree(t, f.repo); !strings.Contains(tree, archPath("p1", "x.md", v0)) || !strings.Contains(tree, "projects/p1/memory/x.md") {
			t.Errorf("tree:\n%s", tree)
		}
		if f.repo.RebaseInProgress() {
			t.Error("wedged")
		}
	}
	if !strings.Contains(logs.String(), "restored 1 note(s)") {
		t.Errorf("no count-only log line:\n%s", logs.String())
	}
	if idx := mustGit(t, b.repo, "show", "HEAD:archive/INDEX.md"); !strings.Contains(idx, "edited after archive, restored") {
		t.Errorf("INDEX.md:\n%s", idx)
	}
	if mustGit(t, b.repo, "rev-parse", "HEAD") != mustGit(t, gitwt.New(b.origin), "rev-parse", "main") {
		t.Error("B did not push")
	}
}

// The mirror image: this host archives while the peer's edit is already pushed.
func TestArchive_ArchiveHere_EditOnPeer_BothKept(t *testing.T) {
	v0, v1 := bigNote+"v0\n", bigNote+"v1 edited\n"
	a := archivePC(t, map[string]string{"x.md": v0})
	b := a.addPC("pc-b")
	b.loop.Cfg.ArchiveDeletes = true
	captureLog(t)

	a.editLocal("x.md", v1)
	a.flushOK() // peer's edit is pushed
	b.userDeletes("x.md")
	b.flushOK() // this host archives the old text and pulls
	a.flushOK()

	for _, f := range []*pullFixture{a, b} {
		claudeHas(t, f, "x.md", v1)
		allArchiveCopiesMatchTheirNames(t, f.repo)
		if tree := headTree(t, f.repo); !strings.Contains(tree, archPath("p1", "x.md", v0)) || !strings.Contains(tree, "projects/p1/memory/x.md") {
			t.Errorf("tree:\n%s", tree)
		}
	}
}

// The live path is taken by different content: the edit goes beside it.
func TestRepairEditedArchives_LivePathOccupied_ConflictCopy(t *testing.T) {
	archClock(t)
	captureLog(t)
	f := newPullFixture(t, map[string]string{"x.md": "other, live\n"})
	f.loop.Cfg.ArchiveDeletes = true
	v0, v1 := bigNote+"v0\n", bigNote+"v1 edited\n"
	p := archPath("p1", "x.md", v0)
	writeRepoFile(t, f.repo, p, v0)
	mustGit(t, f.repo, "add", "-A")
	mustGit(t, f.repo, "commit", "-q", "-m", "archive")
	pre := strings.TrimSpace(mustGit(t, f.repo, "rev-parse", "HEAD"))
	writeRepoFile(t, f.repo, p, v1) // the edit merged into the copy
	mustGit(t, f.repo, "commit", "-qam", "edit merged into the copy")
	post := strings.TrimSpace(mustGit(t, f.repo, "rev-parse", "HEAD"))

	n, err := repairEditedArchives(f.repo, f.loop.Cfg, pre, post)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got := mustGit(t, f.repo, "show", "HEAD:projects/p1/memory/x.md"); got != "other, live\n" {
		t.Errorf("the live note was replaced: %q", got)
	}
	h := sha256.Sum256([]byte(v1))
	cp := "projects/p1/memory/x.conflict-" + hex.EncodeToString(h[:])[:8] + ".md"
	if got := mustGit(t, f.repo, "show", "HEAD:"+cp); got != v1 {
		t.Errorf("conflict copy: %q", got)
	}
	allArchiveCopiesMatchTheirNames(t, f.repo)
	if out := mustGit(t, f.repo, "status", "--porcelain"); strings.TrimSpace(out) != "" {
		t.Errorf("uncommitted leftovers:\n%s", out)
	}
}

// When the old text cannot be found, nothing is left half done: reset and abort.
func TestRepairEditedArchives_CannotRestore_Errors(t *testing.T) {
	archClock(t)
	captureLog(t)
	f := newPullFixture(t, map[string]string{"keep.md": "k\n"})
	f.loop.Cfg.ArchiveDeletes = true
	p := archPath("p1", "x.md", "v0 that nobody committed\n")
	writeRepoFile(t, f.repo, p, "something else\n")
	pre := strings.TrimSpace(mustGit(t, f.repo, "rev-parse", "HEAD"))
	mustGit(t, f.repo, "add", "-A")
	mustGit(t, f.repo, "commit", "-q", "-m", "mismatched copy arrives")
	post := strings.TrimSpace(mustGit(t, f.repo, "rev-parse", "HEAD"))
	if _, err := repairEditedArchives(f.repo, f.loop.Cfg, pre, post); err == nil {
		t.Fatal("want an error when the original bytes are nowhere")
	}
}

// stageHandConflict leaves host B's repo stopped in a rebase that is not the
// daemon's (no pull marker), on the two-host archive conflict.
func stageHandConflict(t *testing.T) *pullFixture {
	t.Helper()
	a := archivePC(t, map[string]string{"x.md": "v0\n"})
	b := a.addPC("pc-b")
	b.loop.Cfg.ArchiveDeletes = true
	a.editLocal("x.md", "v1\n")
	a.flushOK()
	a.userDeletes("x.md")
	a.flushOK()
	b.userDeletes("x.md")
	if _, err := CommitMirror(b.repo, b.loop.Cfg, "b archives"); err != nil {
		t.Fatal(err)
	}
	mustGit(t, b.repo, "fetch", "-q", "origin", "main")
	if _, err := b.repo.Run("rebase", "origin/main"); err == nil || !b.repo.RebaseInProgress() {
		t.Fatalf("setup: expected a stopped rebase, err=%v", err)
	}
	return b
}

// A rebase the user started is never touched by the resolver.
func TestResolveArchiveConflicts_LeavesAUserRebaseAlone(t *testing.T) {
	b := stageHandConflict(t)
	before := mustGit(t, b.repo, "ls-files", "-u")
	n, err := resolveArchiveConflicts(b.repo)
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if !b.repo.RebaseInProgress() || mustGit(t, b.repo, "ls-files", "-u") != before {
		t.Error("the user's rebase was changed")
	}
}

// The same conflict, daemon's own pull, plus an INDEX.md conflict: not in the
// safe set, so the pull aborts as before.
func TestArchive_IndexConflictIsNeverAutoResolved(t *testing.T) {
	a := archivePC(t, map[string]string{"x.md": "v0\n"})
	b := a.addPC("pc-b")
	b.loop.Cfg.ArchiveDeletes = true
	// Host B merges the index as plain text, so two appended lines conflict.
	if err := os.WriteFile(filepath.Join(b.repo.Dir, ".git", "info", "attributes"), []byte("archive/INDEX.md merge\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.editLocal("x.md", "v1\n")
	a.flushOK()
	a.userDeletes("x.md")
	a.flushOK()
	b.userDeletes("x.md")
	err := b.loop.flush(b.repo, b.roots, true)
	if err == nil || !strings.Contains(err.Error(), "rebase conflict") || b.repo.RebaseInProgress() {
		t.Fatalf("got %v, in progress %v", err, b.repo.RebaseInProgress())
	}
}

// The restore commit goes through the scrub like every other commit: an edit
// that holds a secret is held back, nothing is committed, and the caller
// undoes the pull instead of pushing a mismatched copy.
func TestRepairEditedArchives_ScrubHoldsARestoredSecretBack(t *testing.T) {
	archClock(t)
	logs := captureLog(t)
	f := newPullFixture(t, map[string]string{"keep.md": "k\n"})
	f.loop.Cfg.ArchiveDeletes = true
	v0, v1 := bigNote+"v0\n", bigNote+"token: "+fakeSecret()+"\n"
	p := archPath("p1", "x.md", v0)
	writeRepoFile(t, f.repo, p, v0)
	mustGit(t, f.repo, "add", "-A")
	mustGit(t, f.repo, "commit", "-q", "-m", "archive")
	pre := strings.TrimSpace(mustGit(t, f.repo, "rev-parse", "HEAD"))
	writeRepoFile(t, f.repo, p, v1) // as a pull would bring it, unscrubbed
	mustGit(t, f.repo, "commit", "-qam", "edit merged into the copy")
	post := strings.TrimSpace(mustGit(t, f.repo, "rev-parse", "HEAD"))

	if n, err := repairEditedArchives(f.repo, f.loop.Cfg, pre, post); err == nil {
		t.Fatalf("a held-back restore must fail the repair, got n=%d", n)
	}
	if head := strings.TrimSpace(mustGit(t, f.repo, "rev-parse", "HEAD")); head != post {
		t.Error("the restore was committed despite the scrub")
	}
	if strings.Contains(headTree(t, f.repo), "projects/p1/memory/x.md") {
		t.Error("the secret-bearing note reached the tree")
	}
	if strings.Contains(logs.String(), fakeSecret()) {
		t.Error("the secret was logged")
	}
}
