package sync

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

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
		if !e.IsDir() && !strings.HasPrefix(e.Name(), ".") && !strings.Contains(e.Name(), ".from-remote-") {
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
