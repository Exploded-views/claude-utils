package sync

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// sync runs one Reconcile pass the way the daemon does at startup and on the
// pull tick, then refreshes the per-PC manifest as a flush does.
func (m *machine) sync() SyncReport {
	m.t.Helper()
	roots, err := NewRoots(m.cfg)
	if err != nil {
		m.t.Fatal(err)
	}
	man, err := LoadManifest(m.stateDir())
	if err != nil {
		m.t.Fatal(err)
	}
	rep, err := Reconcile(roots, man)
	if err != nil {
		m.t.Fatal(err)
	}
	nm, err := BuildFromClaudeTree(m.claude)
	if err != nil {
		m.t.Fatal(err)
	}
	if err := nm.Save(m.stateDir()); err != nil {
		m.t.Fatal(err)
	}
	return rep
}

// The manifest is per-PC; keep it out of the shared sync dir.
func (m *machine) stateDir() string { return filepath.Join(m.home, "state") }

// remoteDir is where the sync repo keeps the notes of a remote key.
func remoteDir(shared, key string) string {
	return filepath.Join(shared, "remotes", filepath.FromSlash(key), "memory")
}

// pathDir is where the sync repo keeps the notes of a path-keyed folder.
func (m *machine) pathDir(projectDir string) string {
	return filepath.Join(m.cfg.SyncDir, "projects", m.hash(projectDir), "memory")
}

// files lists the memory files in dir, sorted; nil if it does not exist.
func files(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		if !e.IsDir() && !strings.HasPrefix(e.Name(), ".") && !isBackupName(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func eq(a []string, b ...string) bool { return strings.Join(a, ",") == strings.Join(b, ",") }

func TestRemoteSync_SameRemoteAtDifferentPathsSharesNotesBothWays(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	shared := a.cfg.SyncDir
	b := newMachine(t, "bob")
	// Same repository, different checkout locations and URL spellings.
	appA := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:Acme/App.git")
	appB := mkRepo(t, filepath.Join(b.home, "work", "code", "application"), "https://github.com/acme/app.git")

	a.write(appA, "MEMORY.md", "# Mem\n\n## From A\nalpha\n")
	a.write(appA, "feedback_a.md", "note from A\n")
	a.sync()

	if !eq(files(t, remoteDir(shared, appKey)), "MEMORY.md", "feedback_a.md") {
		t.Fatalf("A's notes should land under remotes/: %v", files(t, remoteDir(shared, appKey)))
	}
	if _, err := os.Stat(filepath.Join(shared, "projects", a.hash(appA))); err == nil {
		t.Error("a remote-keyed project must not also be mirrored under its path key")
	}

	b.open(appB) // a session was started there; nothing remembered yet
	b.pullFrom(a)
	b.sync()
	if got, ok := b.read(appB, "feedback_a.md"); !ok || got != "note from A\n" {
		t.Errorf("B did not receive A's memory: %q %v", got, ok)
	}

	b.write(appB, "MEMORY.md", "# Mem\n\n## From B\nbeta\n")
	b.write(appB, "reference_b.md", "note from B\n")
	b.sync()
	a.pullFrom(b)
	a.sync()
	if got, ok := a.read(appA, "reference_b.md"); !ok || got != "note from B\n" {
		t.Errorf("A did not receive B's memory: %q %v", got, ok)
	}
	idx, _ := a.read(appA, "MEMORY.md")
	if !strings.Contains(idx, "From A") || !strings.Contains(idx, "From B") {
		t.Errorf("MEMORY.md should union both PCs' sections, got %q", idx)
	}
}

func TestRemoteSync_FolderWithoutUsableRemoteKeepsItsPathKey(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	shared := a.cfg.SyncDir
	plain := mkdirs(t, a.home, "notes", "scratch")[0]
	noOrigin := mkRepo(t, filepath.Join(a.home, "local-only"), "")
	localRemote := mkRepo(t, filepath.Join(a.home, "localremote"), "/srv/git/x.git")
	for _, d := range []string{plain, noOrigin, localRemote} {
		a.write(d, "note.md", "hi\n")
	}
	a.sync()
	for _, d := range []string{plain, noOrigin, localRemote} {
		if !eq(files(t, a.pathDir(d)), "note.md") {
			t.Errorf("%s should keep the path key", filepath.Base(d))
		}
	}
	if _, err := os.Stat(filepath.Join(shared, "remotes")); err == nil {
		t.Error("no remotes/ folder expected when nothing has a usable remote")
	}
}

func TestRemoteSync_PathModeIgnoresTheRemote(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	shared := a.cfg.SyncDir
	a.cfg.ProjectKey = "path"
	app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	a.write(app, "note.md", "hi\n")
	a.sync()
	if !eq(files(t, a.pathDir(app)), "note.md") {
		t.Error("path mode must use projects/<escaped>/")
	}
	if _, err := os.Stat(filepath.Join(shared, "remotes")); err == nil {
		t.Error("path mode must not create remotes/")
	}
	if _, err := os.Stat(ProjectCachePath(shared)); err == nil {
		t.Error("path mode must not write the project cache")
	}
}

// Deletes, watcher deletes and an rm -rf of memory/ behave in remote mode
// exactly as they do in path mode: the shared copy follows the local one.
func TestRemoteSync_DeleteAndRmRfAreTheSameAsInPathMode(t *testing.T) {
	for _, mode := range []string{"path", "remote"} {
		t.Run(mode, func(t *testing.T) {
			gitEnv(t)
			a := newMachine(t, "alice")
			shared := a.cfg.SyncDir
			a.cfg.ProjectKey = mode
			app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
			mirror := a.pathDir(app)
			if mode == "remote" {
				mirror = remoteDir(shared, appKey)
			}
			memDir := filepath.Join(a.claude, a.hash(app), "memory")
			seed := func() {
				for _, n := range []string{"x.md", "y.md", "z.md"} {
					a.write(app, n, n+"\n")
				}
				a.sync()
				if !eq(files(t, mirror), "x.md", "y.md", "z.md") {
					t.Fatalf("seed: mirror = %v", files(t, mirror))
				}
			}

			// A delete while the daemon was off.
			seed()
			a.remove(app, "x.md")
			a.sync()
			if !eq(files(t, mirror), "y.md", "z.md") {
				t.Errorf("delete: mirror = %v, want y.md z.md", files(t, mirror))
			}

			// A delete seen by the watcher.
			roots, err := NewRoots(a.cfg)
			if err != nil {
				t.Fatal(err)
			}
			l := &Loop{Cfg: a.cfg}
			rmEvent := func(name string) {
				t.Helper()
				ev := fsnotify.Event{Name: filepath.Join(memDir, name), Op: fsnotify.Remove}
				if handled, err := l.handleEvent(nil, roots, ev); err != nil || !handled {
					t.Fatalf("handled=%v err=%v", handled, err)
				}
			}
			a.remove(app, "y.md")
			rmEvent("y.md")
			if !eq(files(t, mirror), "z.md") {
				t.Errorf("watcher delete: mirror = %v, want z.md", files(t, mirror))
			}

			// rm -rf of memory/, as a burst of file removals...
			if err := os.RemoveAll(memDir); err != nil {
				t.Fatal(err)
			}
			rmEvent("z.md")
			if got := files(t, mirror); len(got) != 0 {
				t.Errorf("rm -rf (watcher): mirror = %v, want empty", got)
			}

			// ...and noticed only by the next reconcile.
			seed()
			if err := os.RemoveAll(memDir); err != nil {
				t.Fatal(err)
			}
			a.sync()
			if got := files(t, mirror); len(got) != 0 {
				t.Errorf("rm -rf (reconcile): mirror = %v, want empty", got)
			}
		})
	}
}

// A folder that gains a key is joined to the notes under it, not reconciled
// with them: the shared side wins, nothing is deleted, and the manifest says
// nothing about what the folder had.
func TestRemoteSync_JoinKeepsSharedNotesAndSavesTheLocalOnes(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	b := newMachine(t, "bob")
	appA := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	a.write(appA, "MEMORY.md", "# Mem\n\n## From A\na\n")
	a.write(appA, "same.md", "same\n")
	a.write(appA, "diff.md", "shared\n")
	a.write(appA, "only-shared.md", "s\n")
	a.sync()

	b.pullFrom(a)
	appB := mkRepo(t, filepath.Join(b.home, "code", "app"), "git@github.com:acme/app.git")
	b.write(appB, "MEMORY.md", "# Mem\n\n## From B\nb\n")
	b.write(appB, "same.md", "same\n")
	b.write(appB, "diff.md", "local\n")
	b.write(appB, "only-local.md", "l\n")
	// The manifest remembers a file the folder no longer has; that is not a delete.
	man := NewManifest()
	man.Add(b.hash(appB) + "/memory/only-shared.md")
	if err := man.Save(b.stateDir()); err != nil {
		t.Fatal(err)
	}
	rep := b.sync()

	shared := remoteDir(b.cfg.SyncDir, appKey)
	if !eq(files(t, shared), "MEMORY.md", "diff.md", "only-local.md", "only-shared.md", "same.md") {
		t.Fatalf("shared = %v", files(t, shared))
	}
	if got := readFile(t, filepath.Join(shared, "diff.md")); got != "shared\n" {
		t.Errorf("shared diff.md = %q, want the shared version", got)
	}
	if got, _ := b.read(appB, "diff.md"); got != "shared\n" {
		t.Errorf("local diff.md = %q, want the shared version", got)
	}
	idx := readFile(t, filepath.Join(shared, "MEMORY.md"))
	if !strings.Contains(idx, "From A") || !strings.Contains(idx, "From B") {
		t.Errorf("MEMORY.md was not merged: %q", idx)
	}
	if got, ok := b.read(appB, "only-shared.md"); !ok || got != "s\n" {
		t.Errorf("a shared-only file must reach Claude, got %q,%v", got, ok)
	}
	backups, _ := filepath.Glob(filepath.Join(shared, "diff.md.local-before-join-*"))
	if len(backups) != 1 || readFile(t, backups[0]) != "local\n" {
		t.Errorf("want the local diff.md saved once as .local-before-join-*, got %v", backups)
	}
	if len(rep.BackedUp) != 1 {
		t.Errorf("report.BackedUp = %v", rep.BackedUp)
	}

	// The join happens once: afterwards a delete is a delete again.
	b.remove(appB, "only-local.md")
	b.sync()
	if !eq(files(t, shared), "MEMORY.md", "diff.md", "only-shared.md", "same.md") {
		t.Errorf("after the join a local delete must reach the shared notes, got %v", files(t, shared))
	}
}

// A clone whose Claude folder appears while the daemon runs is joined to the
// shared notes before its first watcher event, so its first MEMORY.md cannot
// overwrite the shared index.
func TestRemoteSync_NewFolderWhileRunningIsJoinedBeforeItsFirstEvent(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	b := newMachine(t, "bob")
	appA := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	a.write(appA, "MEMORY.md", "# Mem\n\n## From A\na\n")
	a.write(appA, "topic.md", "t\n")
	a.sync()

	b.pullFrom(a)
	roots, err := NewRoots(b.cfg) // daemon started; the clone does not exist yet
	if err != nil {
		t.Fatal(err)
	}
	appB := mkRepo(t, filepath.Join(b.home, "code", "app"), "git@github.com:acme/app.git")
	b.write(appB, "MEMORY.md", "# Mem\n\n## From B\nb\n") // Claude's first memory in the new clone
	l := &Loop{Cfg: b.cfg}
	ev := fsnotify.Event{Name: filepath.Join(b.claude, b.hash(appB), "memory", "MEMORY.md"), Op: fsnotify.Create}
	if _, err := l.handleEvent(nil, roots, ev); err != nil {
		t.Fatal(err)
	}
	idx := readFile(t, filepath.Join(remoteDir(b.cfg.SyncDir, appKey), "MEMORY.md"))
	if !strings.Contains(idx, "From A") {
		t.Errorf("shared MEMORY.md lost A's section: %q", idx)
	}
}

// Moving a claim the documented way joins the new holder to the shared notes:
// its stale, same-named note does not replace the current one.
func TestRemoteSync_MovingTheClaimDoesNotLetStaleNotesWin(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	first := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	second := mkRepo(t, filepath.Join(a.home, "src", "app-copy"), "git@github.com:acme/app.git")
	a.write(first, "topic.md", "current\n")
	a.write(second, "topic.md", "stale\n")
	a.sync()
	if got := readFile(t, filepath.Join(remoteDir(a.cfg.SyncDir, appKey), "topic.md")); got != "current\n" {
		t.Fatalf("setup: shared topic.md = %q", got)
	}
	setCacheKey(t, a, first, "path")
	setCacheKey(t, a, second, appKey)
	a.sync()
	if got := readFile(t, filepath.Join(remoteDir(a.cfg.SyncDir, appKey), "topic.md")); got != "current\n" {
		t.Errorf("shared topic.md regressed to %q after the claim moved", got)
	}
}

// commitSyncRepo makes the sync dir a git repo (once) and commits the shared
// notes, as the daemon's flush does. The per-PC .state/ is left out.
func (m *machine) commitSyncRepo(msg string) string {
	m.t.Helper()
	r := gitwt.New(m.cfg.SyncDir)
	if !r.IsRepo() {
		if err := r.Init("main"); err != nil {
			m.t.Fatal(err)
		}
	}
	for _, top := range []string{"projects", "remotes"} {
		if _, err := os.Stat(filepath.Join(m.cfg.SyncDir, top)); err == nil {
			if _, err := r.Run("add", "-A", top); err != nil {
				m.t.Fatal(err)
			}
		}
	}
	if _, err := r.Run("commit", "-q", "--allow-empty", "-m", msg); err != nil {
		m.t.Fatal(err)
	}
	head, err := r.Run("rev-parse", "HEAD")
	if err != nil {
		m.t.Fatal(err)
	}
	return strings.TrimSpace(head)
}

// A folder that comes back to a directory it left imports a file only the
// repo has when that file changed after it left, and leaves a stale one
// alone: nothing but the repo's history can tell it from a note a deleted
// copy left behind. Nothing is removed from the repo: a path-keyed copy may
// still be live on a PC that has not switched.
func TestRemoteSync_ReturningToAMirrorDirImportsOnlyWhatChangedSinceItLeft(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	a.cfg.ProjectKey = "path"
	app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	a.write(app, "x.md", "x\n")
	a.write(app, "z.md", "z\n")
	a.sync()
	a.commitSyncRepo("path mode")

	a.cfg.ProjectKey = "remote"
	a.sync()
	a.commitSyncRepo("remote mode")
	a.remove(app, "z.md")
	a.sync()
	oldCopy := a.pathDir(app)
	if err := os.WriteFile(filepath.Join(oldCopy, "new.md"), []byte("from a path-mode PC\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a.commitSyncRepo("a path-mode PC pushes")

	setCacheKey(t, a, app, "path")
	a.sync()
	if got, ok := a.read(app, "new.md"); !ok || got != "from a path-mode PC\n" {
		t.Errorf("a file added to the old copy after the folder left must be imported, got %q,%v", got, ok)
	}
	if _, ok := a.read(app, "z.md"); ok {
		t.Error("z.md was deleted on this PC and is unchanged in the old copy: it must not come back")
	}
	if !eq(files(t, oldCopy), "new.md", "x.md", "z.md") {
		t.Errorf("nothing may be removed from the old copy, got %v", files(t, oldCopy))
	}
	a.sync() // and it stays that way on the next pass
	if _, ok := a.read(app, "z.md"); ok {
		t.Error("z.md came back on a later pass")
	}
}

// When the repo cannot say what changed since the folder left, nothing that
// only the repo has is imported, and the log says why without a path.
func TestRemoteSync_UnknownLeftAtImportsNothing(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	a.cfg.ProjectKey = "path"
	app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	a.write(app, "x.md", "x\n")
	a.sync()
	a.commitSyncRepo("one")
	hash := a.hash(app)
	// The folder left projects/<hash>/memory at a commit that no longer exists.
	c, _ := LoadProjectCache(a.cfg.SyncDir)
	c.Projects[hash] = ProjectEntry{LeftAt: map[string]string{"projects/" + hash + "/memory": strings.Repeat("ab", 20)}}
	if err := c.Save(a.cfg.SyncDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.pathDir(app), "other.md"), []byte("o\n"), 0600); err != nil {
		t.Fatal(err)
	}
	logs := captureLog(t)
	a.cfg.ProjectKey = "remote"
	setCacheKey(t, a, app, "path")
	a.sync()
	if _, ok := a.read(app, "other.md"); ok {
		t.Error("imported a file although it cannot be told from a stale leftover")
	}
	out := logs.String()
	if !strings.Contains(out, hash) || !strings.Contains(out, "not importing") || strings.Contains(out, a.home) {
		t.Errorf("want a path-free line naming the folder, got %q", out)
	}
}

// A key flip remote -> path -> remote does not resurrect a deleted note
// through the stale projects/<hash> copy left behind by the first switch.
func TestRemoteSync_KeyFlipDoesNotResurrectADelete(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	a.cfg.ProjectKey = "path"
	app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	a.write(app, "x.md", "x\n")
	a.write(app, "z.md", "z\n")
	a.sync() // projects/<hash> has x, z

	a.cfg.ProjectKey = "remote"
	a.sync() // remotes/<key> has x, z; the old copy stays
	a.remove(app, "z.md")
	a.sync()
	if !eq(files(t, remoteDir(a.cfg.SyncDir, appKey)), "x.md") {
		t.Fatalf("setup: shared = %v", files(t, remoteDir(a.cfg.SyncDir, appKey)))
	}

	setCacheKey(t, a, app, "path") // e.g. the documented claim move, or origin removed
	a.sync()
	setCacheKey(t, a, app, "")
	a.sync()
	if got := files(t, remoteDir(a.cfg.SyncDir, appKey)); !eq(got, "x.md") {
		t.Errorf("deleted z.md came back in the shared notes: %v", got)
	}
}

// In a mixed fleet a delete made on the remote-mode PC does not reach a
// path-mode PC that still has the note, and when that PC switches later its
// copy is pushed back. A join never infers a delete from the manifest (the
// folder has no history with the key), so this is the documented reason to
// switch every PC together and to delete no notes until all have switched.
func TestRemoteSync_MixedFleetDeleteIsNotCarriedOverByALaterSwitch(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	b := newMachine(t, "bob")
	a.cfg.ProjectKey, b.cfg.ProjectKey = "path", "path"
	app := mkRepo(t, filepath.Join(t.TempDir(), "src", "app"), "git@github.com:acme/app.git") // same path on both PCs
	a.write(app, "x.md", "x\n")
	a.write(app, "y.md", "y\n")
	a.sync()
	b.open(app)
	b.pullFrom(a)
	b.sync()

	a.cfg.ProjectKey = "remote"
	a.sync()
	a.remove(app, "x.md")
	a.sync()

	b.pullFrom(a)
	b.cfg.ProjectKey = "remote"
	b.sync()
	if got := files(t, remoteDir(b.cfg.SyncDir, appKey)); !eq(got, "x.md", "y.md") {
		t.Errorf("B's switch should push its local-only x.md and delete nothing: shared = %v", got)
	}
}

// Deleting an old clone's Claude project folder while the daemon runs must not
// delete the repo's shared notes for every PC; with the folder gone, a remove
// event for one of its files is not a delete.
func TestHandleEvent_RemovingAnOldCloneFolderDoesNotWipeSharedNotes(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	old := mkRepo(t, filepath.Join(a.home, "src", "old-app"), "git@github.com:acme/app.git")
	a.write(old, "n1.md", "1\n")
	a.write(old, "n2.md", "2\n")
	a.sync()
	roots, err := NewRoots(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	l := &Loop{Cfg: a.cfg}
	_ = os.RemoveAll(old)
	folder := filepath.Join(a.claude, a.hash(old))
	_ = os.RemoveAll(folder)
	for _, n := range []string{"n1.md", "n2.md"} { // what inotify reports for the rm -rf
		ev := fsnotify.Event{Name: filepath.Join(folder, "memory", n), Op: fsnotify.Remove}
		if _, err := l.handleEvent(nil, roots, ev); err != nil {
			t.Fatal(err)
		}
	}
	if got := files(t, remoteDir(a.cfg.SyncDir, appKey)); !eq(got, "n1.md", "n2.md") {
		t.Errorf("shared notes after removing an old clone's folder = %v", got)
	}
}

// The same through the real loop: rm -rf of a remote-keyed folder's Claude
// project folder, with whatever order the watcher reports it in, leaves the
// shared notes in the working tree and in what the loop commits and pushes.
// (rm -rf of just memory/, with the project folder still there, stays a
// delete: see TestRemoteSync_DeleteAndRmRfAreTheSameAsInPathMode.)
func TestLoopRun_RemovingAFolderWithTheLoopRunningKeepsTheSharedNotes(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	a.cfg.DebounceMs = 150
	a.cfg.PullIntervalSec = 3600
	old := mkRepo(t, filepath.Join(a.home, "src", "old-app"), "git@github.com:acme/app.git")
	a.write(old, "n1.md", "1\n")
	a.write(old, "n2.md", "2\n")
	a.write(old, "n3.md", "3\n")

	origin := filepath.Join(t.TempDir(), "origin.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", "-b", "main", origin).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v: %s", err, out)
	}
	sr := gitwt.New(a.cfg.SyncDir)
	if err := sr.Init("main"); err != nil {
		t.Fatal(err)
	}
	if err := sr.SetRemote(origin); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.cfg.SyncDir, ".gitignore"), []byte(".state/\n"), 0600); err != nil {
		t.Fatal(err)
	}

	flushed := make(chan error, 64)
	l := &Loop{Cfg: a.cfg, OnFlush: func(_ bool, err error) { flushed <- err }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()
	wait := func(what string) {
		t.Helper()
		select {
		case err := <-flushed:
			if err != nil {
				t.Fatalf("%s: flush: %v", what, err)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("%s: no flush", what)
		}
	}
	wait("initial flush")
	shared := remoteDir(a.cfg.SyncDir, appKey)
	if !eq(files(t, shared), "n1.md", "n2.md", "n3.md") {
		t.Fatalf("setup: shared = %v", files(t, shared))
	}
	time.Sleep(300 * time.Millisecond) // let the watches settle

	if err := os.RemoveAll(old); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(a.claude, a.hash(old))); err != nil {
		t.Fatal(err)
	}
	wait("after rm -rf")
	time.Sleep(500 * time.Millisecond) // a late event may cause one more flush
	for len(flushed) > 0 {
		<-flushed
	}

	if got := files(t, shared); !eq(got, "n1.md", "n2.md", "n3.md") {
		t.Errorf("shared notes in the working tree after rm -rf = %v", got)
	}
	tree, err := sr.Run("ls-tree", "-r", "--name-only", "HEAD", "--", "remotes")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"n1.md", "n2.md", "n3.md"} {
		if !strings.Contains(tree, "remotes/"+appKey+"/memory/"+n) {
			t.Errorf("%s is no longer in the committed notes: %q", n, tree)
		}
	}
	if out, _ := sr.Run("status", "--porcelain", "--", "remotes"); strings.TrimSpace(out) != "" {
		t.Errorf("remotes/ should match HEAD, status: %q", out)
	}
	// With the folder and the checkout both gone the claim is released too.
	if c, err := LoadProjectCache(a.cfg.SyncDir); err != nil {
		t.Fatal(err)
	} else if e, ok := c.Projects[a.hash(old)]; ok {
		t.Errorf("the vanished folder still holds its claim: %+v", e)
	}
}

// A remote-keyed folder that vanished leaves the old projects/<hash> copy in
// the repo alone too: that copy is no longer this PC's to touch.
func TestRemoteSync_VanishedFolderLeavesTheSupersededCopyAlone(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	old := mkRepo(t, filepath.Join(a.home, "src", "old-app"), "git@github.com:acme/app.git")
	a.write(old, "n1.md", "1\n")
	a.sync()
	if err := os.MkdirAll(a.pathDir(old), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.pathDir(old), "n1.md"), []byte("1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(a.claude, a.hash(old))); err != nil {
		t.Fatal(err)
	}
	a.sync()
	if !eq(files(t, a.pathDir(old)), "n1.md") || !eq(files(t, remoteDir(a.cfg.SyncDir, appKey)), "n1.md") {
		t.Errorf("a vanished folder's repo copies changed: old %v, shared %v",
			files(t, a.pathDir(old)), files(t, remoteDir(a.cfg.SyncDir, appKey)))
	}
}

// A second local clone of a remote that another folder already holds uses its
// path key and stays out of the shared notes, in both directions.
func TestRemoteSync_SecondCloneOnThisPCFallsBackAndStaysIsolated(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	shared := a.cfg.SyncDir
	first := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	second := mkRepo(t, filepath.Join(a.home, "src", "app-copy"), "https://github.com/acme/app")
	a.write(first, "first.md", "1\n")
	a.write(second, "second.md", "2\n")
	a.sync()

	if !eq(files(t, remoteDir(shared, appKey)), "first.md") {
		t.Errorf("shared notes = %v, want only the first folder's", files(t, remoteDir(shared, appKey)))
	}
	if !eq(files(t, a.pathDir(second)), "second.md") {
		t.Errorf("second clone's path-keyed notes = %v", files(t, a.pathDir(second)))
	}
	if _, ok := a.read(second, "first.md"); ok {
		t.Error("the shared notes leaked into the second clone")
	}
	if _, ok := a.read(first, "second.md"); ok {
		t.Error("the second clone's notes leaked into the shared folder's member")
	}

	// A peer's change to the shared notes reaches the member only.
	if err := os.WriteFile(filepath.Join(remoteDir(shared, appKey), "peer.md"), []byte("p\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a.sync()
	if _, ok := a.read(first, "peer.md"); !ok {
		t.Error("the member did not get the peer's note")
	}
	if _, ok := a.read(second, "peer.md"); ok {
		t.Error("the second clone got the peer's note")
	}
}

func TestRemoteSync_FreshCloneOnANewPCIsFilled(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	b := newMachine(t, "bob")
	appA := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	a.write(appA, "MEMORY.md", "# Mem\n\n## A\na\n")
	a.write(appA, "topic.md", "t\n")
	a.sync()

	// B cloned the repo and opened a session: a project folder, no memory/.
	appB := mkRepo(t, filepath.Join(b.home, "code", "app"), "git@github.com:acme/app.git")
	b.open(appB)
	if b.hasMemDir(appB) {
		t.Fatal("setup: B should have no memory/ yet")
	}
	b.pullFrom(a)
	b.sync()
	for _, n := range []string{"MEMORY.md", "topic.md"} {
		if _, ok := b.read(appB, n); !ok {
			t.Errorf("fresh clone was not given %s", n)
		}
	}
}

// Current Claude Code keeps one auto-memory folder per repository, shared by
// all its worktrees; a linked worktree's own folder holds transcripts only.
func TestRemoteSync_LinkedWorktreeStaysPathKeyed(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	shared := a.cfg.SyncDir
	main := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	wt := filepath.Join(a.home, "src", "app-feature")
	addWorktree(t, main, wt)
	a.open(wt)
	a.write(main, "only_main.md", "m\n")
	a.sync()

	if !eq(files(t, remoteDir(shared, appKey)), "only_main.md") {
		t.Errorf("main checkout notes = %v", files(t, remoteDir(shared, appKey)))
	}
	if got := files(t, a.pathDir(wt)); len(got) != 0 {
		t.Errorf("the worktree's path-keyed folder got notes: %v", got)
	}
	if a.hasMemDir(wt) {
		t.Error("the worktree's folder must not get a memory/ dir")
	}
}

func TestRemoteSync_DeletedCheckoutKeepsItsKey(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	shared := a.cfg.SyncDir
	app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	a.write(app, "a.md", "a\n")
	a.sync()
	if err := os.RemoveAll(app); err != nil {
		t.Fatal(err)
	}
	a.write(app, "b.md", "b\n") // the notes keep changing after the checkout is gone
	a.sync()
	if _, err := os.Stat(filepath.Join(shared, "projects", a.hash(app), "memory", "b.md")); err == nil {
		t.Error("deleted checkout re-keyed to its path: notes forked under projects/")
	}
	if !eq(files(t, remoteDir(shared, appKey)), "a.md", "b.md") {
		t.Errorf("deleted checkout should keep its key: %v", files(t, remoteDir(shared, appKey)))
	}
}

func TestRemoteSync_CorruptCacheDoesNotStopTheDaemon(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	shared := a.cfg.SyncDir
	p := ProjectCachePath(shared)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"version":1,"projects":{`), 0600); err != nil {
		t.Fatal(err)
	}
	app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	a.write(app, "a.md", "a\n")
	a.sync() // fails the test if NewRoots errors
	if !eq(files(t, remoteDir(shared, appKey)), "a.md") {
		t.Errorf("daemon should carry on with an empty cache: %v", files(t, remoteDir(shared, appKey)))
	}
}

func TestRemoteSync_InvalidOverrideIsIgnored(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	shared := a.cfg.SyncDir
	app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	a.write(app, "a.md", "a\n")
	c := &ProjectCache{Projects: map[string]ProjectEntry{a.hash(app): {Key: "../../escaped"}}}
	if err := c.Save(shared); err != nil {
		t.Fatal(err)
	}
	captureLog(t)
	a.sync()
	if _, err := os.Stat(filepath.Join(a.home, "escaped")); err == nil {
		t.Error("the override wrote outside the sync dir")
	}
	if !eq(files(t, remoteDir(shared, appKey)), "a.md") {
		t.Errorf("an invalid key should fall back to the derived one: %v", files(t, remoteDir(shared, appKey)))
	}
}

func TestSplitRemotePath(t *testing.T) {
	cases := []struct {
		in        string
		key, name string
		ok        bool
	}{
		{"remotes/github.com/acme/app/memory/MEMORY.md", appKey, "MEMORY.md", true},
		{"remotes/gitlab.example.com/g/sub/app/memory/x.md", "gitlab.example.com/g/sub/app", "x.md", true},
		// A repo that is itself called "memory" is unambiguous.
		{"remotes/github.com/acme/memory/memory/x.md", "github.com/acme/memory", "x.md", true},
		{"projects/-home-a/memory/x.md", "", "", false},
		{"remotes/github.com/memory/x.md", "", "", false}, // key needs host and repo
		{"remotes/github.com/acme/app/x.md", "", "", false},
	}
	for _, c := range cases {
		key, name, ok := splitRemotePath(c.in)
		if ok != c.ok || key != c.key || name != c.name {
			t.Errorf("splitRemotePath(%q) = %q,%q,%v", c.in, key, name, ok)
		}
	}
}

func TestClaudeFolderFor_MapsRepoPathsToTheOneLocalFolder(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	copy2 := mkRepo(t, filepath.Join(a.home, "src", "app-copy"), "git@github.com:acme/app.git")
	plain := mkdirs(t, a.home, "notes")[0]
	for _, d := range []string{app, copy2, plain} {
		a.write(d, "n.md", "x\n")
	}
	roots, err := NewRoots(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		path, hash, name string
		ok               bool
	}{
		{"remotes/github.com/acme/app/memory/n.md", a.hash(app), "n.md", true},
		{"projects/" + a.hash(plain) + "/memory/n.md", a.hash(plain), "n.md", true},
		{"projects/" + a.hash(copy2) + "/memory/n.md", a.hash(copy2), "n.md", true},
		// superseded: this folder is remote-keyed now, so its old copy is not ours
		{"projects/" + a.hash(app) + "/memory/n.md", "", "", false},
		// another PC's folder is path-keyed here too
		{"projects/-home-bob-src-x/memory/n.md", "-home-bob-src-x", "n.md", true},
		// a remote nobody here holds
		{"remotes/github.com/acme/other/memory/n.md", "", "", false},
		{"README.md", "", "", false},
	}
	for _, c := range cases {
		hash, name, ok := claudeFolderFor(roots, c.path)
		if hash != c.hash || name != c.name || ok != c.ok {
			t.Errorf("claudeFolderFor(%q) = %q,%q,%v want %q,%q,%v", c.path, hash, name, ok, c.hash, c.name, c.ok)
		}
	}
	// Path mode never maps a remotes/ path.
	roots.Remote = nil
	if _, _, ok := claudeFolderFor(roots, "remotes/github.com/acme/app/memory/n.md"); ok {
		t.Error("path mode mapped a remotes/ path")
	}
}

func TestPropagateChanges_RemoteKeyedPathsReachTheMemberOnly(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	shared := a.cfg.SyncDir
	main := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	other := mkRepo(t, filepath.Join(a.home, "src", "app-clone"), "https://github.com/acme/app.git")
	a.write(main, "keep.md", "k\n")
	a.write(main, "old.md", "o\n")
	a.write(other, "mine.md", "m\n")
	a.sync()

	// The sync repo itself: two commits, the second as if pulled from a peer.
	sr := gitwt.New(shared)
	if err := sr.Init("main"); err != nil {
		t.Fatal(err)
	}
	if err := sr.AddAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := sr.Commit("one"); err != nil {
		t.Fatal(err)
	}
	pre, _ := sr.Run("rev-parse", "HEAD")
	repoDir := remoteDir(shared, appKey)
	if err := os.WriteFile(filepath.Join(repoDir, "new.md"), []byte("from peer\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repoDir, "old.md")); err != nil {
		t.Fatal(err)
	}
	// A pull also touches the superseded path-keyed copy of the member.
	if err := os.MkdirAll(a.pathDir(main), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.pathDir(main), "stale.md"), []byte("old layout\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := sr.AddAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := sr.Commit("two"); err != nil {
		t.Fatal(err)
	}
	post, _ := sr.Run("rev-parse", "HEAD")

	roots, err := NewRoots(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := propagateChanges(sr, roots, strings.TrimSpace(pre), strings.TrimSpace(post)); err != nil {
		t.Fatal(err)
	}
	if got, ok := a.read(main, "new.md"); !ok || got != "from peer\n" {
		t.Errorf("member: new.md = %q,%v", got, ok)
	}
	if _, ok := a.read(main, "old.md"); ok {
		t.Error("member: old.md should have been removed")
	}
	if _, ok := a.read(main, "stale.md"); ok {
		t.Error("a superseded projects/ copy must not reach the remote-keyed folder")
	}
	if _, ok := a.read(other, "new.md"); ok {
		t.Error("the second clone must not receive the shared notes")
	}
	if _, ok := a.read(other, "mine.md"); !ok {
		t.Error("the second clone lost its own note")
	}
}

func TestHandleEvent_FileGoesToItsFoldersRepoDirectory(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	shared := a.cfg.SyncDir
	main := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	other := mkRepo(t, filepath.Join(a.home, "src", "app-clone"), "git@github.com:acme/app.git")
	a.write(main, "topic.md", "x\n")
	a.write(other, "side.md", "y\n")

	roots, err := NewRoots(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	l := &Loop{Cfg: a.cfg}
	for _, c := range []struct{ dir, name, mirror string }{
		{main, "topic.md", remoteDir(shared, appKey)},
		{other, "side.md", a.pathDir(other)},
	} {
		ev := fsnotify.Event{Name: filepath.Join(a.claude, a.hash(c.dir), "memory", c.name), Op: fsnotify.Write}
		if handled, err := l.handleEvent(nil, roots, ev); err != nil || !handled {
			t.Fatalf("handled=%v err=%v", handled, err)
		}
		if !eq(files(t, c.mirror), c.name) {
			t.Errorf("%s: mirror %s = %v", c.name, c.mirror, files(t, c.mirror))
		}
	}
}

// After switching to remote mode, a folder's old projects/<hash>/memory copy
// stays in the repo and stops being updated; the daemon says so at startup.
func TestRoots_LogSupersededMirrorCopies(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	shared := a.cfg.SyncDir
	app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	fresh := mkRepo(t, filepath.Join(a.home, "src", "fresh"), "git@github.com:acme/fresh.git")
	plain := mkdirs(t, a.home, "notes")[0]
	for _, d := range []string{app, fresh, plain} {
		a.write(d, "n.md", "x\n")
	}
	// Left over from path mode: app and the plain folder have an old copy.
	for _, d := range []string{app, plain} {
		if err := os.MkdirAll(a.pathDir(d), 0700); err != nil {
			t.Fatal(err)
		}
	}

	roots, err := NewRoots(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	logs := captureLog(t)
	roots.LogSuperseded()

	out := logs.String()
	if n := strings.Count(strings.TrimSpace(out), "\n") + 1; n != 1 || !strings.Contains(out, a.hash(app)) {
		t.Errorf("want one line for the switched folder, got %d in %q", n, out)
	}
	if !strings.Contains(out, "no longer updated") || !strings.Contains(out, appKey) {
		t.Errorf("message should say the copy is no longer updated and where it went: %q", out)
	}
	for _, d := range []string{fresh, plain} {
		if strings.Contains(out, a.hash(d)) {
			t.Errorf("%s has nothing superseded in the repo but was logged: %q", filepath.Base(d), out)
		}
	}
	// Path mode has nothing to say.
	logs.Reset()
	Roots{Claude: a.claude, Mirror: filepath.Join(shared, "projects")}.LogSuperseded()
	if logs.Len() != 0 {
		t.Errorf("path mode logged %q", logs.String())
	}
}

// pullFrom models git carrying the repo between PCs: it copies o's mirrored
// notes into m's sync dir, overwriting files and never deleting. The per-PC
// .state/ is not part of the repo and stays behind.
func (m *machine) pullFrom(o *machine) {
	m.t.Helper()
	for _, top := range []string{"projects", "remotes"} {
		src := filepath.Join(o.cfg.SyncDir, top)
		err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			rel, _ := filepath.Rel(o.cfg.SyncDir, p)
			dst := filepath.Join(m.cfg.SyncDir, rel)
			if d.IsDir() {
				return os.MkdirAll(dst, 0700)
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			return writeAtomic(dst, b)
		})
		if err != nil {
			m.t.Fatal(err)
		}
	}
}

func setCacheKey(t *testing.T, m *machine, projectDir, key string) {
	t.Helper()
	c, err := LoadProjectCache(m.cfg.SyncDir)
	if err != nil {
		t.Fatal(err)
	}
	e := c.Projects[m.hash(projectDir)]
	e.Key = key
	c.Projects[m.hash(projectDir)] = e
	if err := c.Save(m.cfg.SyncDir); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, _ := os.ReadFile(p)
	return string(b)
}

// oldIgnore is the .gitignore of a sync dir made before *.local-before-join-*
// was listed in it.
const oldIgnore = "config.json\ndaemon.pid\n.state/\n*.tmp\n*.tmp.*\n*.from-remote-*\ndistilled/DISTILLED.md\n"

// A join's backup of the folder's own note stays on the PC that made it, even
// when the sync dir's tracked .gitignore predates the pattern: it is never
// committed, so it cannot reach another PC's Claude folder.
func TestRemoteSync_JoinBackupNeverReachesGit(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	b := newMachine(t, "bob")
	appA := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	a.write(appA, "diff.md", "shared\n")
	a.sync()
	a.commitSyncRepo("a1")

	b.pullFrom(a)
	appB := mkRepo(t, filepath.Join(b.home, "code", "app"), "git@github.com:acme/app.git")
	b.write(appB, "diff.md", "bob's local, private\n")
	br := gitwt.New(b.cfg.SyncDir)
	if err := br.Init("main"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.cfg.SyncDir, ".gitignore"), []byte(oldIgnore), 0600); err != nil {
		t.Fatal(err)
	}
	b.sync() // daemon start and the join
	backups, _ := filepath.Glob(filepath.Join(remoteDir(b.cfg.SyncDir, appKey), "*.local-before-join-*"))
	if len(backups) != 1 {
		t.Fatalf("setup: want one join backup on disk, got %v", backups)
	}
	if err := br.AddAll(); err != nil { // the flush's staging
		t.Fatal(err)
	}
	if _, err := br.Commit("b1"); err != nil {
		t.Fatal(err)
	}
	tracked, err := br.Run("ls-files")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(tracked, ".local-before-join-") {
		t.Errorf("a join backup is committed with an old .gitignore: %q", tracked)
	}
	if _, err := os.Stat(backups[0]); err != nil {
		t.Errorf("the backup must stay on this PC: %v", err)
	}
}

// The patterns are listed in the repo's .git/info/exclude at startup, once,
// next to whatever is already there.
func TestNewRoots_ListsTheBackupPatternsInInfoExclude(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	if err := gitwt.New(a.cfg.SyncDir).Init("main"); err != nil {
		t.Fatal(err)
	}
	excl := filepath.Join(a.cfg.SyncDir, ".git", "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(excl), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(excl, []byte("mine.txt"), 0600); err != nil { // no trailing newline
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := NewRoots(a.cfg); err != nil {
			t.Fatal(err)
		}
	}
	got := readFile(t, excl)
	for _, want := range []string{"mine.txt\n", "*.local-before-join-*\n", "*.from-remote-*\n"} {
		if strings.Count(got, want) != 1 {
			t.Errorf("info/exclude should list %q once: %q", want, got)
		}
	}
	// A sync dir that is not a repo (yet) is left alone.
	b := newMachine(t, "bob")
	if _, err := NewRoots(b.cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(b.cfg.SyncDir, ".git")); err == nil {
		t.Error("NewRoots must not create a repo")
	}
}

// A backup that an older client pushed is never copied into Claude's tree, in
// either layout, and never read from Claude's tree into the repo.
func TestPropagateChanges_BackupsFromOlderClientsStayOutOfClaude(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	plain := mkdirs(t, a.home, "notes")[0]
	a.write(app, "n.md", "x\n")
	a.write(plain, "n.md", "x\n")
	a.sync()
	pre := a.commitSyncRepo("one")

	roots, err := NewRoots(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	sr := gitwt.New(a.cfg.SyncDir)
	bad := []string{
		filepath.Join(remoteDir(a.cfg.SyncDir, appKey), "n.md.local-before-join-1234"),
		filepath.Join(a.pathDir(plain), "n.md.from-remote-1234"),
		filepath.Join(a.pathDir(plain), "n.md.local-before-join-1234"),
	}
	for _, p := range bad {
		if err := os.WriteFile(p, []byte("private\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := sr.Run("add", "-f", p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sr.Run("commit", "-q", "-m", "from an older client"); err != nil {
		t.Fatal(err)
	}
	post, _ := sr.Run("rev-parse", "HEAD")
	if err := propagateChanges(sr, roots, pre, strings.TrimSpace(post)); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{app, plain} {
		got, _ := filepath.Glob(filepath.Join(a.claude, a.hash(d), "memory", "*-*"))
		if len(got) > 0 {
			t.Errorf("a backup reached the Claude folder: %v", got)
		}
	}
	for _, p := range []string{"remotes/github.com/acme/app/memory/n.md.local-before-join-1", "projects/x/memory/n.md.from-remote-1"} {
		if _, _, ok := claudeFolderFor(roots, p); ok {
			t.Errorf("claudeFolderFor mapped the backup %s", p)
		}
	}
}

// A backup name in Claude's own folder is not a note: Reconcile and a join
// leave it where it is.
func TestReconcile_BackupNamesInClaudeAreNotCopiedToTheMirror(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	plain := mkdirs(t, a.home, "notes")[0]
	for _, d := range []string{app, plain} {
		a.write(d, "n.md", "x\n")
		a.write(d, "n.md.local-before-join-1", "private\n")
		a.write(d, "n.md.from-remote-1", "private\n")
	}
	a.sync() // joins app, reconciles plain
	a.sync()
	for _, dir := range []string{remoteDir(a.cfg.SyncDir, appKey), a.pathDir(plain)} {
		got, _ := filepath.Glob(filepath.Join(dir, "*-*"))
		if len(got) > 0 {
			t.Errorf("a backup name in Claude was copied to %s: %v", dir, got)
		}
	}
}

// A claim released by Refresh and re-claimed by another clone in the same
// Refresh: the join merges into the mirror, and the flush's restore must not
// check remotes/<key>/memory out from HEAD over it. A later pulled change to
// MEMORY.md then must not overwrite the clone's Claude copy.
func TestRemoteSync_RestoreVanishedDoesNotUndoTheJoinOfTheNewHolder(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	first := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	second := mkRepo(t, filepath.Join(a.home, "src", "app-copy"), "git@github.com:acme/app.git")
	a.write(first, "MEMORY.md", "# Mem\n\n## From first\nf\n")
	a.write(second, "MEMORY.md", "# Mem\n\n## From second\ns\n")
	a.sync()
	a.commitSyncRepo("c1")

	// The old clone goes away entirely while the daemon is off.
	_ = os.RemoveAll(first)
	_ = os.RemoveAll(filepath.Join(a.claude, a.hash(first)))

	// Daemon start: NewRoots releases first's claim, second claims the key.
	roots, err := NewRoots(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if roots.Remote.Key(a.hash(second)) != appKey {
		t.Fatalf("setup: second should now hold the key, got %q", roots.Remote.Key(a.hash(second)))
	}
	man, _ := LoadManifest(a.stateDir())
	if _, err := Reconcile(roots, man); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(remoteDir(a.cfg.SyncDir, appKey), "MEMORY.md")
	if !strings.Contains(readFile(t, shared), "From second") {
		t.Fatalf("setup: join did not merge: %q", readFile(t, shared))
	}
	repo := gitwt.New(a.cfg.SyncDir)
	restoreVanished(repo, roots) // first step of the flush
	if got := readFile(t, shared); !strings.Contains(got, "From second") {
		t.Errorf("flush reverted the join's merged MEMORY.md in the mirror: %q", got)
	}
	pre := a.commitSyncRepo("flush")

	// Another PC changes MEMORY.md; the pull brings it and propagateChanges runs.
	if err := os.WriteFile(shared, []byte(readFile(t, shared)+"\n## From bob\nb\n"), 0600); err != nil {
		t.Fatal(err)
	}
	post := a.commitSyncRepo("bob")
	if err := propagateChanges(repo, roots, pre, post); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.read(second, "MEMORY.md"); !strings.Contains(got, "From second") {
		t.Errorf("second clone's own MEMORY.md section is lost from Claude: %q", got)
	}
}

// The same inside one Refresh of a running daemon: the claim is released and
// re-claimed in a single pass, and what gets committed is the join's merge.
func TestRemoteSync_ReleaseAndReclaimInOneRefreshKeepsTheJoinCommitted(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	first := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	a.write(first, "MEMORY.md", "# Mem\n\n## From first\nf\n")
	a.write(first, "n.md", "n\n")
	a.sync()
	a.commitSyncRepo("c1")
	roots, err := NewRoots(a.cfg) // the daemon is running with first as the holder
	if err != nil {
		t.Fatal(err)
	}

	_ = os.RemoveAll(first)
	_ = os.RemoveAll(filepath.Join(a.claude, a.hash(first)))
	shared := remoteDir(a.cfg.SyncDir, appKey)
	if err := os.Remove(filepath.Join(shared, "n.md")); err != nil { // what a racing watcher could have done
		t.Fatal(err)
	}
	second := mkRepo(t, filepath.Join(a.home, "src", "app-copy"), "git@github.com:acme/app.git")
	a.write(second, "MEMORY.md", "# Mem\n\n## From second\ns\n")

	roots.Remote.Refresh() // pull tick: release, then the new clone claims
	man, _ := LoadManifest(a.stateDir())
	if _, err := Reconcile(roots, man); err != nil {
		t.Fatal(err)
	}
	repo := gitwt.New(a.cfg.SyncDir)
	restoreVanished(repo, roots)
	if err := repo.AddAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("flush"); err != nil {
		t.Fatal(err)
	}
	head, err := repo.Run("show", "HEAD:remotes/"+appKey+"/memory/MEMORY.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(head, "From first") || !strings.Contains(head, "From second") {
		t.Errorf("the committed MEMORY.md is not the join's merge: %q", head)
	}
	if got, _ := a.read(second, "n.md"); got != "n\n" {
		t.Errorf("the note deleted by the vanishing folder was not restored before the join, got %q", got)
	}
}

// A holder whose Claude folder is gone but whose checkout is still there has
// its notes restored once, not on every flush; coming back resets that.
func TestRemoteSync_VanishedHolderWithACheckoutIsRestoredOnce(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	a.write(app, "n.md", "n\n")
	a.sync()
	a.commitSyncRepo("c1")
	roots, err := NewRoots(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	repo := gitwt.New(a.cfg.SyncDir)
	note := filepath.Join(remoteDir(a.cfg.SyncDir, appKey), "n.md")
	folder := filepath.Join(a.claude, a.hash(app))

	if err := os.RemoveAll(folder); err != nil { // the checkout stays
		t.Fatal(err)
	}
	_ = os.Remove(note)
	restoreVanished(repo, roots)
	if got := readFile(t, note); got != "n\n" {
		t.Fatalf("the first flush must restore the note, got %q", got)
	}
	_ = os.Remove(note) // a later change in the working tree is left alone
	restoreVanished(repo, roots)
	if _, err := os.Stat(note); err == nil {
		t.Error("the restore ran again on a later flush")
	}

	a.write(app, "n.md", "n\n") // the folder comes back, then goes again
	roots.Remote.Refresh()
	if err := os.RemoveAll(folder); err != nil {
		t.Fatal(err)
	}
	restoreVanished(repo, roots)
	if got := readFile(t, note); got != "n\n" {
		t.Errorf("a second disappearance must be restored too, got %q", got)
	}
}
