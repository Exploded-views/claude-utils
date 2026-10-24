package sync

import (
	"bytes"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

func TestNormalizeRemote(t *testing.T) {
	cases := []struct {
		in   string
		want string // "" means no usable key
	}{
		{"git@github.com:Acme/App.git", "github.com/acme/app"},
		{"https://github.com/acme/app.git", "github.com/acme/app"},
		{"https://github.com/acme/app", "github.com/acme/app"},
		{"https://github.com/acme/app/", "github.com/acme/app"},
		{"HTTPS://GitHub.com/Acme/App.git", "github.com/acme/app"},
		{"https://alice:s3cret@github.com/acme/app.git", "github.com/acme/app"},
		{"ssh://git@github.com/acme/app.git", "github.com/acme/app"},
		{"ssh://git@github.com:2222/acme/app.git", "github.com/acme/app"},
		{"git://example.org/acme/app.git", "example.org/acme/app"},
		{"git@gitlab.example.com:group/sub/app.git", "gitlab.example.com/group/sub/app"},
		{"  git@github.com:acme/app.git\n", "github.com/acme/app"},
		// Not a stable identity across machines.
		{"", ""},
		{"/srv/git/app.git", ""},
		{"file:///srv/git/app.git", ""},
		{"../app", ""},
		{"C:\\repos\\app", ""},
		{"https://github.com/", ""},
		{"https://github.com/onlyowner", ""},
		{"https://github.com/acme/../../etc", ""},
		{"git@github.com:acme/a b.git", "github.com/acme/a_b"},
		// An unescaped '/' in a URL password leaves '@' in the path.
		{"https://alice:pa/ss@github.com/acme/app.git", ""},
		{"git@github.com:acme/app@v2.git", ""},
		// Reserved Windows device names, with or without an extension.
		{"git@github.com:acme/con.git", "github.com/acme/_con"},
		{"git@github.com:acme/aux.txt.git", "github.com/acme/_aux.txt"},
		{"git@github.com:NUL/Com1", "github.com/_nul/_com1"},
		{"git@github.com:acme/lpt9.git", "github.com/acme/_lpt9"},
		{"git@github.com:acme/console.git", "github.com/acme/console"},
		{"git@github.com:acme/com10.git", "github.com/acme/com10"},
	}
	for _, c := range cases {
		got, ok := NormalizeRemote(c.in)
		if c.want == "" {
			if ok {
				t.Errorf("NormalizeRemote(%q) = %q, want no key", c.in, got)
			}
			continue
		}
		if !ok || got != c.want {
			t.Errorf("NormalizeRemote(%q) = %q,%v want %q", c.in, got, ok, c.want)
		}
	}
}

func TestValidKey(t *testing.T) {
	good := []string{"github.com/acme/app", "gitlab.example.com/g/sub/app", "h/r", "a.b/c_d/e-f"}
	bad := []string{"", "path", "github.com", "../../escaped", "a/../b", "a/./b", "/abs/olute", "a//b",
		"a/b/", "Github.com/acme/app", "a/b c", `a\b/c`, "a/b@c", "a:b/c", "a/.."}
	for _, k := range good {
		if !validKey(k) {
			t.Errorf("validKey(%q) = false", k)
		}
	}
	for _, k := range bad {
		if validKey(k) {
			t.Errorf("validKey(%q) = true", k)
		}
	}
}

func TestEscapeProjectPath(t *testing.T) {
	cases := map[string]string{
		"/home/alice/src/app":     "-home-alice-src-app",
		"/home/alice/.config/app": "-home-alice--config-app",
		"/home/alice/my.app":      "-home-alice-my-app",
		`C:\src\foo`:              "C--src-foo",
	}
	for in, want := range cases {
		if got := EscapeProjectPath(in); got != want {
			t.Errorf("EscapeProjectPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// mkdirs creates dirs under root and returns their absolute paths.
func mkdirs(t *testing.T, root string, rels ...string) []string {
	t.Helper()
	var out []string
	for _, r := range rels {
		p := filepath.Join(root, filepath.FromSlash(r))
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func TestWalkCandidates(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "src/app", "src/my-app", "src/my.app", "src/my/app", "src/other", ".hidden/x", "under_score/y", "sp ace/z")

	cases := []struct {
		rest string
		want []string
	}{
		{"src-app", []string{"src/app"}},
		{"src-other", []string{"src/other"}},
		// '-', '.' and '/' all decode from the same escaped character.
		{"src-my-app", []string{"src/my-app", "src/my.app", "src/my/app"}},
		{"-hidden-x", []string{".hidden/x"}},
		{"under-score-y", []string{"under_score/y"}},
		{"sp-ace-z", []string{"sp ace/z"}},
		{"src-missing", nil},
		{"src", []string{"src"}},
	}
	for _, c := range cases {
		got := walkCandidates(root, c.rest)
		var want []string
		for _, w := range c.want {
			want = append(want, filepath.Join(root, filepath.FromSlash(w)))
		}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("walkCandidates(%q) = %v, want %v", c.rest, got, want)
		}
	}
}

func TestWalkCandidates_FollowsSymlinkedDirs(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "real/app")
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "link")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	got := walkCandidates(root, "link-app")
	if len(got) != 1 || got[0] != filepath.Join(root, "link", "app") {
		t.Errorf("got %v", got)
	}
}

func TestCandidates_FromFilesystemRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix-style absolute path")
	}
	root := t.TempDir()
	dir := mkdirs(t, root, "src/app.v2")[0]
	got := Candidates(EscapeProjectPath(dir))
	if len(got) != 1 || got[0] != dir {
		t.Errorf("Candidates(%q) = %v, want [%s]", EscapeProjectPath(dir), got, dir)
	}
}

// ---- end-to-end behavior with real git checkouts ----

func gitEnv(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
}

// mkRepo creates dir as a git checkout whose origin is url (none if empty).
func mkRepo(t *testing.T, dir, url string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	r := gitwt.New(dir)
	if err := r.Init("main"); err != nil {
		t.Fatal(err)
	}
	if url != "" {
		if _, err := r.Run("remote", "add", "origin", url); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// machine is one simulated PC: its own ~/.claude/projects and project
// checkouts, sharing a sync work-tree with the other machine (as if git
// had already carried the repo contents between them).
type machine struct {
	t      *testing.T
	home   string
	claude string
	cfg    config.Config
}

func newMachine(t *testing.T, name, shared string) *machine {
	t.Helper()
	home := filepath.Join(t.TempDir(), name)
	cfg := config.Defaults()
	cfg.SyncDir = shared
	cfg.ClaudeProjectsDir = filepath.Join(home, ".claude", "projects")
	cfg.ProjectKey = "remote"
	if err := os.MkdirAll(cfg.ClaudeProjectsDir, 0700); err != nil {
		t.Fatal(err)
	}
	// The manifest is per-PC; keep it out of the shared dir.
	return &machine{t: t, home: home, claude: cfg.ClaudeProjectsDir, cfg: cfg}
}

func (m *machine) hash(projectDir string) string { return EscapeProjectPath(projectDir) }

// open makes the project folder Claude creates when a session starts there.
func (m *machine) open(projectDir string) {
	m.t.Helper()
	if err := os.MkdirAll(filepath.Join(m.claude, m.hash(projectDir)), 0700); err != nil {
		m.t.Fatal(err)
	}
}

// openMem makes the folder and its memory/ dir, as a project Claude Code has
// already written auto memory for (even if it is empty now).
func (m *machine) openMem(projectDir string) {
	m.t.Helper()
	if err := os.MkdirAll(filepath.Join(m.claude, m.hash(projectDir), "memory"), 0700); err != nil {
		m.t.Fatal(err)
	}
}

func (m *machine) hasMemDir(projectDir string) bool {
	_, err := os.Stat(filepath.Join(m.claude, m.hash(projectDir), "memory"))
	return err == nil
}

func (m *machine) remove(projectDir, name string) {
	m.t.Helper()
	if err := os.Remove(filepath.Join(m.claude, m.hash(projectDir), "memory", name)); err != nil {
		m.t.Fatal(err)
	}
}

func (m *machine) write(projectDir, name, content string) {
	m.t.Helper()
	writeMem(m.t, m.claude, m.hash(projectDir), name, content)
}

func (m *machine) read(projectDir, name string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(m.claude, m.hash(projectDir), "memory", name))
	return string(b), err == nil
}

// sync runs one reconcile pass, then commits the sync repo and refreshes the
// per-PC manifest the way the daemon does after a flush. The commit matters:
// the committed copy is the merge base for the next pass.
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
	repo := gitwt.New(m.cfg.SyncDir)
	if err := os.MkdirAll(m.cfg.SyncDir, 0700); err != nil {
		m.t.Fatal(err)
	}
	if !repo.IsRepo() {
		if err := repo.Init("main"); err != nil {
			m.t.Fatal(err)
		}
	}
	if err := repo.AddAll(); err != nil {
		m.t.Fatal(err)
	}
	if _, err := repo.Commit("sync"); err != nil {
		m.t.Fatal(err)
	}
	return rep
}

func (m *machine) stateDir() string { return filepath.Join(m.home, "state") }

func TestRemoteKey_SameRepoAtDifferentPathsSharesMemory(t *testing.T) {
	gitEnv(t)
	shared := t.TempDir()
	a := newMachine(t, "alice", shared)
	b := newMachine(t, "bob", shared)
	// Same repository, different checkout locations and URL spellings.
	appA := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:Acme/App.git")
	appB := mkRepo(t, filepath.Join(b.home, "work", "code", "application"), "https://github.com/acme/app.git")

	a.write(appA, "MEMORY.md", "# Mem\n\n## From A\nalpha\n")
	a.write(appA, "feedback_a.md", "note from A\n")
	a.sync()

	if _, err := os.Stat(filepath.Join(shared, "remotes", "github.com", "acme", "app", "memory", "feedback_a.md")); err != nil {
		t.Fatalf("expected repo-side folder keyed by remote: %v", err)
	}
	if _, err := os.Stat(filepath.Join(shared, "projects", a.hash(appA))); err == nil {
		t.Error("remote-keyed project must not also be mirrored under its path key")
	}

	// B has written memory for the repo before (its memory/ exists), but is empty.
	b.openMem(appB)
	b.sync()
	if got, ok := b.read(appB, "feedback_a.md"); !ok || got != "note from A\n" {
		t.Errorf("B did not receive A's memory: %q %v", got, ok)
	}

	// And back the other way, including a MEMORY.md union.
	b.write(appB, "MEMORY.md", "# Mem\n\n## From B\nbeta\n")
	b.write(appB, "reference_b.md", "note from B\n")
	b.sync()
	a.sync()
	if got, ok := a.read(appA, "reference_b.md"); !ok || got != "note from B\n" {
		t.Errorf("A did not receive B's memory: %q %v", got, ok)
	}
	idx, _ := a.read(appA, "MEMORY.md")
	if !strings.Contains(idx, "From A") || !strings.Contains(idx, "From B") {
		t.Errorf("MEMORY.md should union both machines' sections, got %q", idx)
	}
}

func TestRemoteKey_NonRepoKeepsPathKey(t *testing.T) {
	gitEnv(t)
	shared := t.TempDir()
	a := newMachine(t, "alice", shared)
	plain := mkdirs(t, a.home, "notes", "scratch")[0] // not a git checkout
	noOrigin := mkRepo(t, filepath.Join(a.home, "local-only"), "")
	localRemote := mkRepo(t, filepath.Join(a.home, "localremote"), "/srv/git/x.git")

	for _, d := range []string{plain, noOrigin, localRemote} {
		a.write(d, "note.md", "hi\n")
	}
	a.sync()
	for _, d := range []string{plain, noOrigin, localRemote} {
		if _, err := os.Stat(filepath.Join(shared, "projects", a.hash(d), "memory", "note.md")); err != nil {
			t.Errorf("%s should keep the path key: %v", d, err)
		}
	}
	if _, err := os.Stat(filepath.Join(shared, "remotes")); err == nil {
		t.Error("no remotes/ folder expected when nothing has a usable remote")
	}
}

func TestRemoteKey_PathModeIsUnchanged(t *testing.T) {
	gitEnv(t)
	shared := t.TempDir()
	a := newMachine(t, "alice", shared)
	a.cfg.ProjectKey = "path"
	app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	a.write(app, "note.md", "hi\n")
	a.sync()
	if _, err := os.Stat(filepath.Join(shared, "projects", a.hash(app), "memory", "note.md")); err != nil {
		t.Errorf("path mode must mirror by path key: %v", err)
	}
	if _, err := os.Stat(filepath.Join(shared, "remotes")); err == nil {
		t.Error("path mode must not create remotes/")
	}
}

func TestRemoteKey_SubdirectoryOfRepoKeepsPathKey(t *testing.T) {
	gitEnv(t)
	shared := t.TempDir()
	a := newMachine(t, "alice", shared)
	repo := mkRepo(t, filepath.Join(a.home, "mono"), "git@github.com:acme/mono.git")
	sub := mkdirs(t, repo, "packages/web")[0]
	a.write(sub, "note.md", "hi\n")
	a.write(repo, "root.md", "hi\n")
	a.sync()
	if _, err := os.Stat(filepath.Join(shared, "projects", a.hash(sub), "memory", "note.md")); err != nil {
		t.Errorf("a subdirectory is not the repo and keeps its path key: %v", err)
	}
	if _, err := os.Stat(filepath.Join(shared, "remotes", "github.com", "acme", "mono", "memory", "root.md")); err != nil {
		t.Errorf("repo root should be remote-keyed: %v", err)
	}
}

// addWorktree makes a linked worktree of repo at dir.
func addWorktree(t *testing.T, repo, dir string) {
	t.Helper()
	r := gitwt.New(repo)
	if _, err := r.Run("commit", "--allow-empty", "-q", "-m", "root"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run("worktree", "add", "-q", "-b", "feature", dir); err != nil {
		t.Fatal(err)
	}
}

// Current Claude Code keeps one auto-memory folder per repository, shared by
// all its worktrees; a worktree's own project folder holds transcripts only.
// So a linked worktree is not keyed by the remote, never receives copies, and
// never gets a memory/ dir from us.
func TestRemoteKey_LinkedWorktreeFolderStaysTranscriptOnly(t *testing.T) {
	gitEnv(t)
	shared := t.TempDir()
	a := newMachine(t, "alice", shared)
	main := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	wt := filepath.Join(a.home, "src", "app-feature")
	addWorktree(t, main, wt)
	a.open(wt)
	a.write(main, "MEMORY.md", "# Mem\n\n## Main\nfrom main\n")
	a.write(main, "only_main.md", "m\n")
	a.sync()

	if a.hasMemDir(wt) {
		t.Error("the worktree's folder must not get a memory/ dir")
	}
	m, err := NewRemoteMap(shared, a.claude)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Key(a.hash(wt)); got != "" {
		t.Errorf("a linked worktree must not be keyed by remote, got %q", got)
	}
	if got := m.Key(a.hash(main)); got != "github.com/acme/app" {
		t.Errorf("main checkout key = %q", got)
	}
	if _, err := os.Stat(filepath.Join(shared, "remotes", "github.com", "acme", "app", "memory", "only_main.md")); err != nil {
		t.Errorf("main checkout's memory should land in the remote-keyed folder: %v", err)
	}
	if _, err := os.Stat(filepath.Join(shared, "projects", a.hash(wt), "memory", "only_main.md")); err == nil {
		t.Error("main's notes must not be copied under the worktree's path key")
	}
}

// With no worktree copies to fight the real folder, deletes and MEMORY.md
// pruning in it stick, and a plain edit leaves no conflict backup.
func TestRemoteKey_LinkedWorktreeDoesNotUndoPruning(t *testing.T) {
	gitEnv(t)
	shared := t.TempDir()
	a := newMachine(t, "alice", shared)
	main := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	wt := filepath.Join(a.home, "src", "app-wt")
	addWorktree(t, main, wt)
	a.open(wt) // transcripts-only folder, as Claude Code creates it
	a.write(main, "MEMORY.md", "# Mem\n\n## Keep\nkeep me\n\n## Stale\nobsolete note\n")
	a.write(main, "old_topic.md", "obsolete\n")
	a.write(main, "topic.md", "v1\n")
	a.sync()

	// Claude prunes in the real memory folder; the watcher mirrors it.
	roots, err := NewRoots(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	a.write(main, "MEMORY.md", "# Mem\n\n## Keep\nkeep me\n")
	if err := CopyToMirror(roots, a.hash(main), "MEMORY.md"); err != nil {
		t.Fatal(err)
	}
	a.remove(main, "old_topic.md")
	if err := RemoveFromMirror(roots, a.hash(main), "old_topic.md"); err != nil {
		t.Fatal(err)
	}
	a.write(main, "topic.md", "v2\n")
	if err := CopyToMirror(roots, a.hash(main), "topic.md"); err != nil {
		t.Fatal(err)
	}
	rep := a.sync()

	if idx, _ := a.read(main, "MEMORY.md"); strings.Contains(idx, "Stale") {
		t.Errorf("removed MEMORY.md section resurrected in the live folder:\n%s", idx)
	}
	if _, ok := a.read(main, "old_topic.md"); ok {
		t.Error("deleted topic file resurrected in the live folder")
	}
	if len(rep.BackedUp) > 0 {
		t.Errorf("a plain edit produced conflict backups: %v", rep.BackedUp)
	}
	if a.hasMemDir(wt) {
		t.Error("the worktree's folder must not get a memory/ dir")
	}
}

// A folder with no memory/ dir is not a group member: it is not filled in and
// not given a memory/ dir.
func TestRemoteKey_FolderWithoutMemoryDirIsNotFilled(t *testing.T) {
	gitEnv(t)
	shared := t.TempDir()
	a := newMachine(t, "alice", shared)
	b := newMachine(t, "bob", shared)
	appA := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	appB := mkRepo(t, filepath.Join(b.home, "code", "app"), "git@github.com:acme/app.git")
	a.write(appA, "note.md", "hi\n")
	a.sync()
	b.open(appB)
	b.sync()
	if b.hasMemDir(appB) {
		t.Error("a folder without memory/ must not get one")
	}
	b.openMem(appB)
	b.sync()
	if got, ok := b.read(appB, "note.md"); !ok || got != "hi\n" {
		t.Errorf("once memory/ exists the folder is filled in: %q %v", got, ok)
	}
}

// twoClones is one PC with two clones of the same remote, each with a memory/
// dir (each is a real, separate Claude auto-memory folder).
func twoClones(t *testing.T) (a *machine, c1, c2, repoDir string) {
	t.Helper()
	gitEnv(t)
	shared := t.TempDir()
	a = newMachine(t, "alice", shared)
	c1 = mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	c2 = mkRepo(t, filepath.Join(a.home, "src", "app2"), "https://github.com/acme/app.git")
	a.openMem(c1)
	a.openMem(c2)
	return a, c1, c2, filepath.Join(shared, "remotes", "github.com", "acme", "app", "memory")
}

func noBackups(t *testing.T, repoDir string, rep SyncReport) {
	t.Helper()
	if len(rep.BackedUp) > 0 {
		t.Errorf("unexpected conflict backups: %v", rep.BackedUp)
	}
	if m, _ := filepath.Glob(filepath.Join(repoDir, "*.from-remote-*")); len(m) > 0 {
		t.Errorf("unexpected backup files: %v", m)
	}
}

func TestRemoteKey_TwoClonesFirstSyncUnions(t *testing.T) {
	a, c1, c2, repoDir := twoClones(t)
	a.write(c1, "MEMORY.md", "# Mem\n\n## One\nfrom one\n")
	a.write(c1, "only_one.md", "1\n")
	a.write(c2, "MEMORY.md", "# Mem\n\n## Two\nfrom two\n")
	a.write(c2, "only_two.md", "2\n")
	a.sync() // no committed base yet

	for _, d := range []string{c1, c2} {
		for _, f := range []string{"only_one.md", "only_two.md"} {
			if _, ok := a.read(d, f); !ok {
				t.Errorf("%s missing %s after union", filepath.Base(d), f)
			}
		}
		idx, _ := a.read(d, "MEMORY.md")
		if !strings.Contains(idx, "## One") || !strings.Contains(idx, "## Two") {
			t.Errorf("%s MEMORY.md not unioned: %q", filepath.Base(d), idx)
		}
	}
	if _, err := os.Stat(filepath.Join(repoDir, "only_two.md")); err != nil {
		t.Errorf("both clones share the repo folder: %v", err)
	}
}

// The committed copy is the merge base: the clone that still equals it is
// stale and takes the edit, with no backup.
func TestRemoteKey_TwoClonesPlainEditLeavesNoBackup(t *testing.T) {
	a, c1, c2, repoDir := twoClones(t)
	a.write(c1, "topic.md", "v1\n")
	a.write(c1, "MEMORY.md", "# Mem\n\n## A\nalpha\n")
	a.sync()
	if got, _ := a.read(c2, "topic.md"); got != "v1\n" {
		t.Fatalf("c2 should have received the file, got %q", got)
	}

	a.write(c1, "topic.md", "v2\n")
	a.write(c1, "MEMORY.md", "# Mem\n\n## A\nalpha\n\n## B\nbeta\n")
	rep := a.sync()
	noBackups(t, repoDir, rep)
	if got, _ := a.read(c2, "topic.md"); got != "v2\n" {
		t.Errorf("stale clone should take the edit, got %q", got)
	}
	if got, _ := a.read(c2, "MEMORY.md"); !strings.Contains(got, "## B") {
		t.Errorf("stale clone's MEMORY.md should take the edit, got %q", got)
	}

	// And the other direction: the later clone edits, the earlier one is stale.
	a.write(c2, "topic.md", "v3\n")
	rep = a.sync()
	noBackups(t, repoDir, rep)
	if got, _ := a.read(c1, "topic.md"); got != "v3\n" {
		t.Errorf("c1 = %q, want v3", got)
	}
}

func TestRemoteKey_TwoClonesDeletePropagates(t *testing.T) {
	a, c1, c2, repoDir := twoClones(t)
	a.write(c1, "topic.md", "x\n")
	a.write(c1, "keep.md", "k\n")
	a.sync()
	a.remove(c1, "topic.md")
	rep := a.sync()
	if _, ok := a.read(c2, "topic.md"); ok {
		t.Error("a delete in one clone must delete the file in the other")
	}
	if _, ok := a.read(c1, "topic.md"); ok {
		t.Error("the deleted file came back in the clone that deleted it")
	}
	if _, err := os.Stat(filepath.Join(repoDir, "topic.md")); err == nil {
		t.Error("delete should propagate to the repo folder")
	}
	if len(rep.RemovedFromMirror) != 1 {
		t.Errorf("RemovedFromMirror = %v", rep.RemovedFromMirror)
	}
	if _, ok := a.read(c2, "keep.md"); !ok {
		t.Error("unrelated file lost")
	}
	noBackups(t, repoDir, rep)

	// The same from the second clone, and it stays deleted on a further pass.
	a.remove(c2, "keep.md")
	a.sync()
	a.sync()
	if _, ok := a.read(c1, "keep.md"); ok {
		t.Error("delete in the second clone did not reach the first")
	}
}

// An edit made since the base beats a delete in the other clone: no data loss.
func TestRemoteKey_TwoClonesEditBeatsDelete(t *testing.T) {
	a, c1, c2, repoDir := twoClones(t)
	a.write(c1, "topic.md", "v1\n")
	a.sync()
	a.write(c1, "topic.md", "v2\n")
	a.remove(c2, "topic.md")
	rep := a.sync()
	for _, d := range []string{c1, c2} {
		if got, _ := a.read(d, "topic.md"); got != "v2\n" {
			t.Errorf("%s = %q, want the edited file kept", filepath.Base(d), got)
		}
	}
	noBackups(t, repoDir, rep)
}

func TestRemoteKey_TwoClonesPruningSticks(t *testing.T) {
	for _, watcher := range []bool{false, true} {
		name := "reconcile"
		if watcher {
			name = "watcher mirrored first"
		}
		t.Run(name, func(t *testing.T) {
			a, c1, c2, repoDir := twoClones(t)
			a.write(c1, "MEMORY.md", "# Mem\n\n## Keep\nkeep me\n\n## Stale\nobsolete note\n")
			a.write(c1, "old_topic.md", "obsolete\n")
			a.sync()
			if idx, _ := a.read(c2, "MEMORY.md"); !strings.Contains(idx, "Stale") {
				t.Fatalf("c2 should have the full index first: %q", idx)
			}

			a.write(c1, "MEMORY.md", "# Mem\n\n## Keep\nkeep me\n")
			a.remove(c1, "old_topic.md")
			if watcher {
				roots, err := NewRoots(a.cfg)
				if err != nil {
					t.Fatal(err)
				}
				if err := CopyToMirror(roots, a.hash(c1), "MEMORY.md"); err != nil {
					t.Fatal(err)
				}
				if err := RemoveFromMirror(roots, a.hash(c1), "old_topic.md"); err != nil {
					t.Fatal(err)
				}
			}
			rep := a.sync()
			for _, d := range []string{c1, c2} {
				if idx, _ := a.read(d, "MEMORY.md"); strings.Contains(idx, "Stale") || !strings.Contains(idx, "Keep") {
					t.Errorf("%s: pruned section resurrected or index damaged:\n%s", filepath.Base(d), idx)
				}
				if _, ok := a.read(d, "old_topic.md"); ok {
					t.Errorf("%s: deleted topic file resurrected", filepath.Base(d))
				}
			}
			noBackups(t, repoDir, rep)
			// Stable on the next pass too.
			noBackups(t, repoDir, a.sync())
		})
	}
}

// Real divergence (both clones edited since the base) still falls back to the
// existing rules: MEMORY.md merges, anything else keeps the newest and backs
// the other up.
func TestRemoteKey_TwoClonesDivergentEditsMergeOrBackUp(t *testing.T) {
	a, c1, c2, repoDir := twoClones(t)
	a.write(c1, "topic.md", "v1\n")
	a.write(c1, "MEMORY.md", "# Mem\n\n## Base\nbase\n")
	a.sync()

	a.write(c1, "topic.md", "older edit\n")
	a.write(c2, "topic.md", "newer edit\n")
	past := mustTime(t, "2020-01-01T00:00:00Z")
	if err := os.Chtimes(filepath.Join(a.claude, a.hash(c1), "memory", "topic.md"), past, past); err != nil {
		t.Fatal(err)
	}
	a.write(c1, "MEMORY.md", "# Mem\n\n## Base\nbase\n\n## One\nfrom one\n")
	a.write(c2, "MEMORY.md", "# Mem\n\n## Base\nbase\n\n## Two\nfrom two\n")

	rep := a.sync()
	for _, d := range []string{c1, c2} {
		if got, _ := a.read(d, "topic.md"); got != "newer edit\n" {
			t.Errorf("%s topic.md = %q, want the newest edit", filepath.Base(d), got)
		}
		idx, _ := a.read(d, "MEMORY.md")
		if !strings.Contains(idx, "## One") || !strings.Contains(idx, "## Two") {
			t.Errorf("%s MEMORY.md should merge both edits: %q", filepath.Base(d), idx)
		}
	}
	if len(rep.BackedUp) != 1 {
		t.Errorf("BackedUp = %v, want exactly the losing topic.md", rep.BackedUp)
	}
	matches, _ := filepath.Glob(filepath.Join(repoDir, "topic.md.from-remote-*"))
	if len(matches) != 1 {
		t.Fatalf("want one backup, got %v", matches)
	}
	if b, _ := os.ReadFile(matches[0]); string(b) != "older edit\n" {
		t.Errorf("backup = %q", b)
	}
}

func TestRemoteKey_SingleFolderDeletePropagates(t *testing.T) {
	gitEnv(t)
	shared := t.TempDir()
	a := newMachine(t, "alice", shared)
	app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	a.write(app, "topic.md", "x\n")
	a.sync()
	if err := os.Remove(filepath.Join(a.claude, a.hash(app), "memory", "topic.md")); err != nil {
		t.Fatal(err)
	}
	a.sync()
	if _, err := os.Stat(filepath.Join(shared, "remotes", "github.com", "acme", "app", "memory", "topic.md")); err == nil {
		t.Error("delete should propagate to the repo folder")
	}
}

// ---- ambiguous decoding and the per-machine cache ----

func TestRemoteKey_AmbiguousDecodeFallsBackToPathKeyAndRecordsCandidates(t *testing.T) {
	gitEnv(t)
	shared := t.TempDir()
	a := newMachine(t, "alice", shared)
	dashed := mkRepo(t, filepath.Join(a.home, "src", "my-app"), "git@github.com:acme/one.git")
	mkRepo(t, filepath.Join(a.home, "src", "my.app"), "git@github.com:acme/two.git")

	// Claude only knows the escaped name; both directories produce it.
	hash := a.hash(dashed)
	writeMem(t, a.claude, hash, "note.md", "hi\n")
	a.sync()

	if _, err := os.Stat(filepath.Join(shared, "projects", hash, "memory", "note.md")); err != nil {
		t.Errorf("ambiguous project must keep the path key: %v", err)
	}
	c, err := LoadProjectCache(a.cfg.SyncDir)
	if err != nil {
		t.Fatal(err)
	}
	e := c.Projects[hash]
	if e.Dir != "" || len(e.Candidates) != 2 {
		t.Errorf("want 2 recorded candidates and no dir, got %+v", e)
	}

	// The user pins the right directory by hand; the next pass uses it.
	e.Dir = dashed
	c.Projects[hash] = e
	if err := c.Save(a.cfg.SyncDir); err != nil {
		t.Fatal(err)
	}
	a.sync()
	if _, err := os.Stat(filepath.Join(shared, "remotes", "github.com", "acme", "one", "memory", "note.md")); err != nil {
		t.Errorf("pinned dir should switch the project to its remote key: %v", err)
	}
}

func TestRemoteKey_AmbiguousButSameRemoteResolves(t *testing.T) {
	gitEnv(t)
	shared := t.TempDir()
	a := newMachine(t, "alice", shared)
	d1 := mkRepo(t, filepath.Join(a.home, "src", "my-app"), "git@github.com:acme/app.git")
	mkRepo(t, filepath.Join(a.home, "src", "my.app"), "https://github.com/acme/app.git")
	a.write(d1, "note.md", "hi\n")
	a.sync()
	if _, err := os.Stat(filepath.Join(shared, "remotes", "github.com", "acme", "app", "memory", "note.md")); err != nil {
		t.Errorf("candidates that agree on the remote are not ambiguous: %v", err)
	}
}

func TestRemoteKey_UnresolvableKeepsPathKey(t *testing.T) {
	gitEnv(t)
	// The project directory no longer exists on this machine.
	shared := t.TempDir()
	a := newMachine(t, "alice", shared)
	writeMem(t, a.claude, "-gone-away-project", "note.md", "hi\n")
	a.sync()
	if _, err := os.Stat(filepath.Join(shared, "projects", "-gone-away-project", "memory", "note.md")); err != nil {
		t.Errorf("unresolvable project keeps path key: %v", err)
	}
}

func TestRemoteKey_CacheKeyOverrideWins(t *testing.T) {
	gitEnv(t)
	shared := t.TempDir()
	a := newMachine(t, "alice", shared)
	app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	other := mkRepo(t, filepath.Join(a.home, "src", "fork"), "git@github.com:me/fork.git")
	a.write(app, "a.md", "a\n")
	a.write(other, "b.md", "b\n")

	c := &ProjectCache{Projects: map[string]ProjectEntry{
		a.hash(app):   {Key: "path"},                // keep per-path on purpose
		a.hash(other): {Key: "github.com/acme/app"}, // fold the fork into upstream's folder
	}}
	if err := c.Save(a.cfg.SyncDir); err != nil {
		t.Fatal(err)
	}
	a.sync()
	if _, err := os.Stat(filepath.Join(shared, "projects", a.hash(app), "memory", "a.md")); err != nil {
		t.Errorf("key \"path\" should force the path key: %v", err)
	}
	if _, err := os.Stat(filepath.Join(shared, "remotes", "github.com", "acme", "app", "memory", "b.md")); err != nil {
		t.Errorf("explicit key should be used verbatim: %v", err)
	}
}

func TestRemoteKey_InvalidCacheKeyOverrideIsRejected(t *testing.T) {
	gitEnv(t)
	shared := filepath.Join(t.TempDir(), "sync")
	a := newMachine(t, "alice", shared)
	app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	a.write(app, "a.md", "a\n")
	c := &ProjectCache{Projects: map[string]ProjectEntry{a.hash(app): {Key: "../../escaped"}}}
	if err := c.Save(shared); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	a.sync()
	if _, err := os.Stat(filepath.Join(filepath.Dir(shared), "escaped", "memory", "a.md")); err == nil {
		t.Error("cache key ../../escaped wrote memory outside the sync dir")
	}
	// Rejected override: fall back to what the checkout says.
	if _, err := os.Stat(filepath.Join(shared, "remotes", "github.com", "acme", "app", "memory", "a.md")); err != nil {
		t.Errorf("an invalid key should fall back to the derived remote key: %v", err)
	}
	if !strings.Contains(logs.String(), "../../escaped") {
		t.Errorf("rejected override should be logged, got %q", logs.String())
	}
}

// Deleting a checkout must not flip its key back to the path key and fork its
// notes into projects/<hash>/.
func TestRemoteKey_DeletedCheckoutKeepsItsKey(t *testing.T) {
	gitEnv(t)
	shared := t.TempDir()
	a := newMachine(t, "alice", shared)
	app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	a.write(app, "a.md", "a\n")
	a.sync()
	if err := os.RemoveAll(app); err != nil {
		t.Fatal(err)
	}
	a.write(app, "b.md", "b\n") // the notes keep changing after the checkout is gone
	a.sync()
	if _, err := os.Stat(filepath.Join(shared, "projects", a.hash(app))); err == nil {
		t.Error("deleted checkout re-keyed to path: notes duplicated under projects/<hash>/")
	}
	if _, err := os.Stat(filepath.Join(shared, "remotes", "github.com", "acme", "app", "memory", "b.md")); err != nil {
		t.Errorf("deleted checkout should keep its remote key: %v", err)
	}
}

// Only positive evidence re-keys a folder: git ran and said there is no origin
// (or a different one). Anything that stops git answering keeps the last key.
func TestRemoteKey_StickyKeyOnlyChangesOnPositiveEvidence(t *testing.T) {
	gitEnv(t)
	shared := t.TempDir()
	a := newMachine(t, "alice", shared)
	app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	a.write(app, "a.md", "a\n")
	hash := a.hash(app)

	m, err := NewRemoteMap(shared, a.claude)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Key(hash); got != "github.com/acme/app" {
		t.Fatalf("initial key = %q", got)
	}
	steps := []struct {
		name string
		url  string
		ok   bool
		want string
	}{
		{"git error (dubious ownership, not installed, ...)", "", false, "github.com/acme/app"},
		{"git error again", "", false, "github.com/acme/app"},
		{"different origin", "git@github.com:acme/other.git", true, "github.com/acme/other"},
		{"git error after the change", "", false, "github.com/acme/other"},
		{"git says there is no origin", "", true, ""},
	}
	for _, st := range steps {
		m.Lookup = func(string) (string, bool) { return st.url, st.ok }
		m.Refresh()
		if got := m.Key(hash); got != st.want {
			t.Errorf("%s: key = %q, want %q", st.name, got, st.want)
		}
	}

	// The last key survives a restart: it lives in the cache, not in memory.
	m.Lookup = func(string) (string, bool) { return "git@github.com:acme/app.git", true }
	m.Refresh()
	m2, err := NewRemoteMap(shared, a.claude)
	if err != nil {
		t.Fatal(err)
	}
	m2.Lookup = func(string) (string, bool) { return "", false }
	m2.Refresh()
	if got := m2.Key(hash); got != "github.com/acme/app" {
		t.Errorf("after restart with git failing, key = %q", got)
	}
}

func TestGitOrigin(t *testing.T) {
	gitEnv(t)
	root := t.TempDir()
	withOrigin := mkRepo(t, filepath.Join(root, "with"), "git@github.com:acme/app.git")
	noOrigin := mkRepo(t, filepath.Join(root, "without"), "")
	sub := mkdirs(t, withOrigin, "pkg")[0]
	plain := mkdirs(t, root, "plain")[0]

	cases := []struct {
		name string
		dir  string
		url  string
		ok   bool
	}{
		{"checkout with origin", withOrigin, "git@github.com:acme/app.git", true},
		{"checkout without origin", noOrigin, "", true},
		{"subdirectory of a checkout", sub, "", true},
		{"not a repository", plain, "", true},
		{"missing directory", filepath.Join(root, "gone"), "", false},
	}
	for _, c := range cases {
		url, ok := gitOrigin(c.dir)
		if url != c.url || ok != c.ok {
			t.Errorf("%s: gitOrigin = %q,%v want %q,%v", c.name, url, ok, c.url, c.ok)
		}
	}
}

func TestRemoteKey_CacheRecordsResolvedDirAndSurvivesRestart(t *testing.T) {
	gitEnv(t)
	shared := t.TempDir()
	a := newMachine(t, "alice", shared)
	app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	a.write(app, "a.md", "a\n")
	a.sync()
	c, err := LoadProjectCache(a.cfg.SyncDir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Projects[a.hash(app)].Dir != app {
		t.Errorf("cache should record the resolved dir, got %+v", c.Projects[a.hash(app)])
	}
	// The cache must live under .state/ so it is never committed.
	if !strings.Contains(filepath.ToSlash(ProjectCachePath(a.cfg.SyncDir)), "/.state/") {
		t.Errorf("cache path %s should be under .state/", ProjectCachePath(a.cfg.SyncDir))
	}
}

// A corrupt (hand-edited or torn) projects.json must not stop the daemon: it is
// set aside and resolution starts from an empty cache.
func TestRemoteKey_CorruptCacheIsQuarantined(t *testing.T) {
	gitEnv(t)
	shared := t.TempDir()
	a := newMachine(t, "alice", shared)
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

	matches, _ := filepath.Glob(p + ".corrupt-*")
	if len(matches) != 1 {
		t.Fatalf("want the corrupt file renamed aside, got %v", matches)
	}
	if b, _ := os.ReadFile(matches[0]); string(b) != `{"version":1,"projects":{` {
		t.Errorf("quarantined content = %q", b)
	}
	if _, err := os.Stat(filepath.Join(shared, "remotes", "github.com", "acme", "app", "memory", "a.md")); err != nil {
		t.Errorf("daemon should carry on with an empty cache: %v", err)
	}
	c, err := LoadProjectCache(shared)
	if err != nil || c.Projects[a.hash(app)].Dir != app {
		t.Errorf("fresh cache should have been written: %+v, %v", c, err)
	}
}

func TestLoadProjectCache_MissingIsEmpty(t *testing.T) {
	c, err := LoadProjectCache(t.TempDir())
	if err != nil || len(c.Projects) != 0 {
		t.Errorf("got %+v, %v", c, err)
	}
}

// ---- pull propagation ----

func TestSplitRemotePath(t *testing.T) {
	cases := []struct {
		in        string
		key, name string
		ok        bool
	}{
		{"remotes/github.com/acme/app/memory/MEMORY.md", "github.com/acme/app", "MEMORY.md", true},
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

func TestPropagateChanges_RemoteKeyedPathsReachEveryLocalFolder(t *testing.T) {
	gitEnv(t)
	shared := t.TempDir()
	a := newMachine(t, "alice", shared)
	main := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	wt := mkRepo(t, filepath.Join(a.home, "src", "app-clone"), "https://github.com/acme/app.git")
	a.openMem(wt)
	a.write(main, "keep.md", "k\n")
	a.write(main, "old.md", "o\n")
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
	repoDir := filepath.Join(shared, "remotes", "github.com", "acme", "app", "memory")
	if err := os.WriteFile(filepath.Join(repoDir, "new.md"), []byte("from peer\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repoDir, "old.md")); err != nil {
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
	for _, d := range []string{main, wt} {
		if got, ok := a.read(d, "new.md"); !ok || got != "from peer\n" {
			t.Errorf("%s: new.md = %q,%v", filepath.Base(d), got, ok)
		}
		if _, ok := a.read(d, "old.md"); ok {
			t.Errorf("%s: old.md should have been removed", filepath.Base(d))
		}
	}
}

// ---- watcher events ----

func TestHandleEvent_RemoteKeyedFileGoesToSharedFolder(t *testing.T) {
	gitEnv(t)
	shared := t.TempDir()
	a := newMachine(t, "alice", shared)
	main := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	wt := mkRepo(t, filepath.Join(a.home, "src", "app-clone"), "https://github.com/acme/app.git")
	a.openMem(wt)
	a.write(main, "topic.md", "x\n")

	roots, err := NewRoots(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	l := &Loop{Cfg: a.cfg}
	ev := fsnotify.Event{Name: filepath.Join(a.claude, a.hash(main), "memory", "topic.md"), Op: fsnotify.Write}
	handled, err := l.handleEvent(nil, roots, ev)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if _, err := os.Stat(filepath.Join(shared, "remotes", "github.com", "acme", "app", "memory", "topic.md")); err != nil {
		t.Errorf("event should mirror into the remote-keyed folder: %v", err)
	}
	if !l.needReconcile {
		t.Error("two folders share the key, so the sibling needs a reconcile")
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
