package sync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	gosync "sync"
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
		// Windows drops trailing dots from a name, so two spellings would
		// otherwise share one directory there and not elsewhere.
		{"https://github.com/acme/app./", "github.com/acme/app"},
		{"git@github.com:acme/app...git", "github.com/acme/app"},
		{"git@github.com:acme/con..git", "github.com/acme/_con"},
		{"https://github.com/acme/...", ""},
		{"https://github.com/acme/ok/...", ""},
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
		"a/b/", "Github.com/acme/app", "a/b c", `a\b/c`, "a/b@c", "a:b/c", "a/..",
		// Not what NormalizeRemote can produce, and not usable on Windows.
		"github.com/acme/app.", "a./b", "github.com/acme/...", "github.com/acme/con", "github.com/aux.txt/app",
		"github.com/lpt1/x"}
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

// fakeEntry is a directory entry whose type is not a plain file or directory
// (a Windows junction reads as irregular).
type fakeEntry struct {
	name string
	typ  fs.FileMode
}

func (f fakeEntry) Name() string               { return f.name }
func (f fakeEntry) IsDir() bool                { return false }
func (f fakeEntry) Type() fs.FileMode          { return f.typ }
func (f fakeEntry) Info() (fs.FileInfo, error) { return nil, errors.New("unused") }

func TestIsDirEntry_StatsSymlinksAndIrregularEntries(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "real")
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, typ := range []fs.FileMode{fs.ModeSymlink, fs.ModeIrregular} {
		if !isDirEntry(root, fakeEntry{"real", typ}) {
			t.Errorf("type %v pointing at a directory should count as one", typ)
		}
		if isDirEntry(root, fakeEntry{"file", typ}) {
			t.Errorf("type %v pointing at a file is not a directory", typ)
		}
		if isDirEntry(root, fakeEntry{"missing", typ}) {
			t.Errorf("type %v pointing nowhere is not a directory", typ)
		}
	}
	if isDirEntry(root, fakeEntry{"real", 0}) {
		t.Error("a plain non-directory entry is not a directory")
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

// Windows and macOS file systems are case-insensitive by default, so the
// folder name Claude derived from the path as typed can differ in case from
// the entry on disk.
func TestWalkCandidates_FoldsCaseWhereTheFileSystemDoes(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "Src/App", "src/Other")
	for goos, want := range map[string]int{"linux": 0, "windows": 1, "darwin": 1} {
		got := walkCandidatesFold(root, "src-app", foldFor(goos))
		if len(got) != want {
			t.Errorf("%s: walkCandidatesFold = %v, want %d match(es)", goos, got, want)
		}
	}
	// Same-named siblings that differ only in case are all candidates.
	mkdirs(t, root, "SRC/app")
	if got := walkCandidatesFold(root, "src-app", foldFor("windows")); len(got) != 2 {
		t.Errorf("got %v, want both Src/App and SRC/app", got)
	}
}

// The "C--" drive branch of Candidates, driven through an injected drive root
// so it runs on Linux.
func TestCandidatesIn_WindowsDriveBranch(t *testing.T) {
	root := t.TempDir()
	dir := mkdirs(t, root, "Src/Foo.Bar")[0]
	env := candidateRoots{
		Drive: func(letter string) string {
			if letter != "C" {
				t.Errorf("drive letter = %q", letter)
			}
			return root
		},
		Fold: foldFor("windows"),
	}
	got := candidatesIn(`C--src-foo-bar`, env)
	if len(got) != 1 || got[0] != dir {
		t.Errorf("candidatesIn = %v, want [%s]", got, dir)
	}
	if got := candidatesIn(`C--nothing-here`, env); len(got) != 0 {
		t.Errorf("got %v", got)
	}
	// A drive name needs a drive root; without one nothing resolves.
	if got := candidatesIn(`C--src-foo-bar`, candidateRoots{}); got != nil {
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

// A repo's own folder is always its root folder (linked worktrees and
// subdirectories are keyed by path), so a fresh clone on a new PC, whose folder
// has no memory/ yet, is filled in from the shared notes.
func TestRemoteKey_FreshCloneWithoutMemoryDirIsFilled(t *testing.T) {
	gitEnv(t)
	shared := t.TempDir()
	a := newMachine(t, "alice", shared)
	b := newMachine(t, "bob", shared)
	appA := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	appB := mkRepo(t, filepath.Join(b.home, "code", "app"), "git@github.com:acme/app.git")
	a.write(appA, "note.md", "hi\n")
	a.sync()
	b.open(appB) // a session was started here; no memory/ yet
	b.sync()
	if got, ok := b.read(appB, "note.md"); !ok || got != "hi\n" {
		t.Errorf("a folder with no memory/ should be filled in: %q %v", got, ok)
	}
}

// Only a key resolved on this run fills a folder. One merely remembered from an
// earlier run (the checkout is gone, git will not answer) only keeps syncing
// what it already has.
func TestRemoteKey_StickyKeyDoesNotFillAFolder(t *testing.T) {
	gitEnv(t)
	shared := t.TempDir()
	a := newMachine(t, "alice", shared)
	b := newMachine(t, "bob", shared)
	appA := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	a.write(appA, "note.md", "hi\n")
	a.sync()

	gone := filepath.Join(b.home, "code", "gone")
	b.open(gone) // the checkout does not exist on this PC
	c := &ProjectCache{Projects: map[string]ProjectEntry{b.hash(gone): {LastKey: "github.com/acme/app"}}}
	if err := c.Save(shared); err != nil {
		t.Fatal(err)
	}
	b.sync()
	if b.hasMemDir(gone) {
		t.Error("a folder keyed only by lastKey must not be filled in")
	}
	// An explicit key in the cache is deliberate: it fills.
	c.Projects[b.hash(gone)] = ProjectEntry{Key: "github.com/acme/app"}
	if err := c.Save(shared); err != nil {
		t.Fatal(err)
	}
	b.sync()
	if got, ok := b.read(gone, "note.md"); !ok || got != "hi\n" {
		t.Errorf("a key override should fill the folder: %q %v", got, ok)
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

// reconcileOnly is machine.sync without the commit: the state when a commit
// did not include the mirror change (scrub held it back, the denylist policy
// blocked the commit, or the daemon stopped before committing).
func (m *machine) reconcileOnly() SyncReport {
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

// commitShared commits the shared work-tree as a pull would have left it.
func commitShared(t *testing.T, dir string) {
	t.Helper()
	r := gitwt.New(dir)
	if err := r.AddAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("pulled"); err != nil {
		t.Fatal(err)
	}
}

func backupWith(t *testing.T, repoDir, name, content string) bool {
	t.Helper()
	ms, _ := filepath.Glob(filepath.Join(repoDir, name+".from-remote-*"))
	for _, m := range ms {
		if b, _ := os.ReadFile(m); string(b) == content {
			return true
		}
	}
	return false
}

// A pull moved HEAD but the members were not brought to it (daemon stopped or
// crashed between pull and propagate, or CopyToClaude failed, e.g. a locked
// file on Windows). The members still equal what this PC last synced, so they
// are stale and take the pulled edit; HEAD is not the merge base.
func TestRemoteKey_PulledButNotPropagatedIsNotReverted(t *testing.T) {
	t.Run("two clones", func(t *testing.T) {
		a, c1, _, repoDir := twoClones(t)
		a.write(c1, "topic.md", "v1\n")
		a.write(c1, "MEMORY.md", "# Mem\n\n## A\nalpha\n")
		a.sync()
		if err := os.WriteFile(filepath.Join(repoDir, "topic.md"), []byte("remote v2\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repoDir, "MEMORY.md"), []byte("# Mem\n\n## A\nalpha\n\n## R\nremote\n"), 0600); err != nil {
			t.Fatal(err)
		}
		commitShared(t, a.cfg.SyncDir)
		a.sync()
		got, _ := a.read(c1, "topic.md")
		if got != "remote v2\n" && !backupWith(t, repoDir, "topic.md", "remote v2\n") {
			t.Errorf("remote edit of topic.md lost with no backup: clone=%q", got)
		}
		if idx, _ := a.read(c1, "MEMORY.md"); !strings.Contains(idx, "## R") {
			t.Errorf("remote MEMORY.md section dropped: %q", idx)
		}
	})
	t.Run("single clone (control: path rules keep it)", func(t *testing.T) {
		gitEnv(t)
		shared := t.TempDir()
		a := newMachine(t, "alice", shared)
		app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
		repoDir := filepath.Join(shared, "remotes", "github.com", "acme", "app", "memory")
		a.write(app, "topic.md", "v1\n")
		a.write(app, "MEMORY.md", "# Mem\n\n## A\nalpha\n")
		a.sync()
		_ = os.WriteFile(filepath.Join(repoDir, "topic.md"), []byte("remote v2\n"), 0600)
		_ = os.WriteFile(filepath.Join(repoDir, "MEMORY.md"), []byte("# Mem\n\n## A\nalpha\n\n## R\nremote\n"), 0600)
		commitShared(t, shared)
		a.sync()
		got, _ := a.read(app, "topic.md")
		if got != "remote v2\n" && !backupWith(t, repoDir, "topic.md", "remote v2\n") {
			t.Errorf("control: remote edit lost: %q", got)
		}
		if idx, _ := a.read(app, "MEMORY.md"); !strings.Contains(idx, "## R") {
			t.Errorf("control: remote section dropped: %q", idx)
		}
	})
}

// HEAD holds an old version (a file held back from commits keeps it): a delete
// in one clone still sticks, because the other clone equals this PC's record.
func TestRemoteKey_DeleteSticksWhenHeadHoldsAnOldVersion(t *testing.T) {
	a, c1, c2, repoDir := twoClones(t)
	a.write(c1, "topic.md", "v0\n")
	a.sync() // HEAD: v0
	a.write(c1, "topic.md", "v1 token\n")
	a.reconcileOnly() // c2 and the mirror get v1; not committed
	if got, _ := a.read(c2, "topic.md"); got != "v1 token\n" {
		t.Fatalf("setup: c2 = %q", got)
	}
	roots, _ := NewRoots(a.cfg)
	a.remove(c1, "topic.md")
	_ = RemoveFromMirror(roots, a.hash(c1), "topic.md")
	a.reconcileOnly()
	a.reconcileOnly()
	for _, d := range []string{c1, c2} {
		if _, ok := a.read(d, "topic.md"); ok {
			t.Errorf("%s: deleted file resurrected", filepath.Base(d))
		}
	}
	if _, err := os.Stat(filepath.Join(repoDir, "topic.md")); err == nil {
		t.Error("deleted file back in the mirror")
	}
}

// Same old HEAD: c1 prunes MEMORY.md back to exactly the HEAD version. That
// must not look stale; c2's un-pruned copy equals the record and takes the prune.
func TestRemoteKey_PruningSticksWhenHeadHoldsAnOldVersion(t *testing.T) {
	a, c1, c2, _ := twoClones(t)
	a.write(c1, "MEMORY.md", "# Mem\n\n## Keep\nk\n")
	a.sync() // HEAD
	a.write(c1, "MEMORY.md", "# Mem\n\n## Keep\nk\n\n## New\nn\n")
	a.reconcileOnly()
	roots, _ := NewRoots(a.cfg)
	a.write(c1, "MEMORY.md", "# Mem\n\n## Keep\nk\n")
	_ = CopyToMirror(roots, a.hash(c1), "MEMORY.md")
	a.reconcileOnly()
	for _, d := range []string{c1, c2} {
		if idx, _ := a.read(d, "MEMORY.md"); strings.Contains(idx, "## New") {
			t.Errorf("%s: pruned section resurrected: %q", filepath.Base(d), idx)
		}
	}
}

// Nothing committed for the file (e.g. a new held-back one): an edit in one
// clone and a delete in the other within one debounce window. The watcher has
// already removed the mirror copy, but the edit differs from the record, so the
// edit wins.
func TestRemoteKey_EditBeatsDeleteWithNothingCommitted(t *testing.T) {
	a, c1, c2, _ := twoClones(t)
	a.write(c1, "topic.md", "v1\n")
	a.reconcileOnly() // c2 gets v1; nothing committed
	roots, _ := NewRoots(a.cfg)
	a.write(c2, "topic.md", "v2 edit\n")
	_ = CopyToMirror(roots, a.hash(c2), "topic.md")
	a.remove(c1, "topic.md")
	_ = RemoveFromMirror(roots, a.hash(c1), "topic.md")
	a.reconcileOnly()
	if got, ok := a.read(c2, "topic.md"); !ok || got != "v2 edit\n" {
		t.Errorf("edit made since the last sync was deleted: %q %v", got, ok)
	}
}

// withOrigin gives the sync repo at shared a local bare origin, as the daemon
// has, and returns the origin's path. .state/ is ignored as init arranges.
func withOrigin(t *testing.T, shared string) string {
	t.Helper()
	origin := filepath.Join(t.TempDir(), "origin.git")
	if out, err := exec.Command("git", "init", "--bare", "-q", "-b", "main", origin).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v: %s", err, out)
	}
	repo := gitwt.New(shared)
	if err := repo.Init("main"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shared, ".gitignore"), []byte(".state/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetRemote(origin); err != nil {
		t.Fatal(err)
	}
	return origin
}

// peerPush is another PC: it clones origin, changes the given repo files
// (a nil content deletes the file) and pushes.
func peerPush(t *testing.T, origin string, files map[string]*string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "peer")
	peer := gitwt.New(dir)
	if err := peer.Clone(origin); err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if content == nil {
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(p, []byte(*content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := peer.AddAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.Commit("peer"); err != nil {
		t.Fatal(err)
	}
	if err := peer.Push("main"); err != nil {
		t.Fatal(err)
	}
}

func str(s string) *string { return &s }

const appMem = "remotes/github.com/acme/app/memory/"

// The daemon dies after the pull moved HEAD and before propagateChanges ran.
// On restart the members still equal this PC's record while the work-tree has
// the pulled files, so the pull is brought in rather than overwritten.
func TestRemoteKey_CrashBetweenPullAndPropagateIsRecovered(t *testing.T) {
	a, c1, c2, repoDir := twoClones(t)
	origin := withOrigin(t, a.cfg.SyncDir)
	a.write(c1, "topic.md", "v1\n")
	a.write(c1, "gone.md", "g\n")
	a.write(c1, "MEMORY.md", "# Mem\n\n## A\nalpha\n\n## Old\nold\n")
	a.sync()
	sr := gitwt.New(a.cfg.SyncDir)
	if err := sr.Push("main"); err != nil {
		t.Fatal(err)
	}
	peerPush(t, origin, map[string]*string{
		appMem + "topic.md":  str("remote v2\n"),
		appMem + "gone.md":   nil,
		appMem + "MEMORY.md": str("# Mem\n\n## A\nalpha\n"),
	})
	if err := sr.Pull("main"); err != nil {
		t.Fatal(err)
	}
	// ...crash: propagateChanges never ran.
	if got, _ := a.read(c1, "topic.md"); got != "v1\n" {
		t.Fatalf("setup: c1 = %q", got)
	}

	rep := a.reconcileOnly() // the daemon's startup reconcile
	for _, d := range []string{c1, c2} {
		if got, _ := a.read(d, "topic.md"); got != "remote v2\n" {
			t.Errorf("%s: topic.md = %q, want the pulled edit", filepath.Base(d), got)
		}
		if _, ok := a.read(d, "gone.md"); ok {
			t.Errorf("%s: pulled delete not applied", filepath.Base(d))
		}
		if idx, _ := a.read(d, "MEMORY.md"); strings.Contains(idx, "## Old") {
			t.Errorf("%s: pulled pruning not applied: %q", filepath.Base(d), idx)
		}
	}
	if _, err := os.Stat(filepath.Join(repoDir, "gone.md")); err == nil {
		t.Error("the pulled delete was undone in the work-tree")
	}
	noBackups(t, repoDir, rep)
}

// propagateChanges records what it brought to the members, so a later delete
// is judged against the pulled content and not against an older sync.
func TestRemoteKey_PropagatedPullIsRecorded(t *testing.T) {
	a, c1, c2, _ := twoClones(t)
	origin := withOrigin(t, a.cfg.SyncDir)
	a.write(c1, "topic.md", "v1\n")
	a.sync()
	sr := gitwt.New(a.cfg.SyncDir)
	if err := sr.Push("main"); err != nil {
		t.Fatal(err)
	}
	peerPush(t, origin, map[string]*string{appMem + "topic.md": str("remote v2\n")})

	roots, err := NewRoots(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	l := &Loop{Cfg: a.cfg, Branch: "main", Hostname: "alice"}
	if err := l.flush(sr, roots, true); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{c1, c2} {
		if got, _ := a.read(d, "topic.md"); got != "remote v2\n" {
			t.Fatalf("%s: topic.md = %q after the pull", filepath.Base(d), got)
		}
	}
	a.remove(c1, "topic.md")
	a.sync()
	for _, d := range []string{c1, c2} {
		if _, ok := a.read(d, "topic.md"); ok {
			t.Errorf("%s: delete after a pull was treated as an edit and undone", filepath.Base(d))
		}
	}
}

// A pulled change meets a local edit of the same file: a true conflict, so
// MEMORY.md merges and the loser of any other file is kept as a backup.
func TestRemoteKey_PullConflictingWithLocalEditMergesOrBacksUp(t *testing.T) {
	a, c1, c2, repoDir := twoClones(t)
	a.write(c1, "topic.md", "v1\n")
	a.write(c1, "MEMORY.md", "# Mem\n\n## Base\nbase\n")
	a.sync()
	roots, err := NewRoots(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	pulled := func(name, content string) {
		if err := os.WriteFile(filepath.Join(repoDir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	pulled("topic.md", "remote edit\n")
	pulled("MEMORY.md", "# Mem\n\n## Base\nbase\n\n## Remote\nr\n")
	a.write(c2, "topic.md", "local edit\n")
	a.write(c2, "MEMORY.md", "# Mem\n\n## Base\nbase\n\n## Local\nl\n")
	sr := gitwt.New(a.cfg.SyncDir)
	if err := sr.AddAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := sr.Commit("pulled"); err != nil {
		t.Fatal(err)
	}
	if err := propagateChanges(sr, roots, "HEAD~1", "HEAD"); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{c1, c2} {
		idx, _ := a.read(d, "MEMORY.md")
		if !strings.Contains(idx, "## Remote") || !strings.Contains(idx, "## Local") {
			t.Errorf("%s: MEMORY.md should merge both: %q", filepath.Base(d), idx)
		}
		if got, _ := a.read(d, "topic.md"); got != "local edit\n" {
			t.Errorf("%s: topic.md = %q, want the local edit kept", filepath.Base(d), got)
		}
	}
	if !backupWith(t, repoDir, "topic.md", "remote edit\n") {
		t.Error("the pulled version should be kept as a backup")
	}
}

// With no record for a file nothing can be judged deleted, even if the
// manifest remembers it in a folder that now lacks it.
func TestRemoteKey_NoRecordNeverDeletes(t *testing.T) {
	a, c1, c2, _ := twoClones(t)
	a.write(c2, "topic.md", "kept\n")
	man := NewManifest()
	man.Add(a.hash(c1) + "/memory/topic.md")
	roots, err := NewRoots(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Reconcile(roots, man); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{c1, c2} {
		if got, _ := a.read(d, "topic.md"); got != "kept\n" {
			t.Errorf("%s: topic.md = %q, want it kept without a record", filepath.Base(d), got)
		}
	}
}

// Without a record, differing non-index copies keep the newest and back up the
// others; the index is merged with an empty base.
func TestRemoteKey_NoRecordNewestWinsWithBackup(t *testing.T) {
	a, c1, c2, repoDir := twoClones(t)
	a.write(c1, "topic.md", "older\n")
	a.write(c2, "topic.md", "newer\n")
	past := mustTime(t, "2020-01-01T00:00:00Z")
	if err := os.Chtimes(filepath.Join(a.claude, a.hash(c1), "memory", "topic.md"), past, past); err != nil {
		t.Fatal(err)
	}
	a.sync()
	for _, d := range []string{c1, c2} {
		if got, _ := a.read(d, "topic.md"); got != "newer\n" {
			t.Errorf("%s: topic.md = %q", filepath.Base(d), got)
		}
	}
	if !backupWith(t, repoDir, "topic.md", "older\n") {
		t.Error("the older copy should be backed up")
	}
}

// A clone that appears later has no manifest entries, so what it lacks was
// never received, not deleted.
func TestRemoteKey_NewMemberIsFilledNotTreatedAsDeleter(t *testing.T) {
	a, c1, c2, repoDir := twoClones(t)
	a.write(c1, "topic.md", "x\n")
	a.sync()
	c3 := mkRepo(t, filepath.Join(a.home, "src", "app3"), "git@github.com:acme/app.git")
	a.openMem(c3)
	rep := a.sync()
	for _, d := range []string{c1, c2, c3} {
		if got, _ := a.read(d, "topic.md"); got != "x\n" {
			t.Errorf("%s: topic.md = %q", filepath.Base(d), got)
		}
	}
	noBackups(t, repoDir, rep)
}

func TestSyncState_RoundTripAndBase(t *testing.T) {
	dir := t.TempDir()
	const group = "remotes/github.com/acme/app/memory"
	st := LoadSyncState(dir)
	if err := st.Record(group, "MEMORY.md", []byte("idx\n")); err != nil {
		t.Fatal(err)
	}
	if err := st.Record(group, "topic.md", []byte("t\n")); err != nil {
		t.Fatal(err)
	}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	st = LoadSyncState(dir)
	if h, ok := st.Hash(group, "topic.md"); !ok || h != contentHash([]byte("t\n")) {
		t.Errorf("Hash = %q,%v", h, ok)
	}
	if got := st.Base(group, "MEMORY.md"); string(got) != "idx\n" {
		t.Errorf("Base(MEMORY.md) = %q", got)
	}
	if got := st.Base(group, "topic.md"); got != nil {
		t.Errorf("only the index keeps base bytes, got %q", got)
	}
	// Base bytes that do not match the recorded hash (a crash between the base
	// write and the state write) are not a base.
	bp := filepath.Join(dir, ".state", "base", filepath.FromSlash(group), "MEMORY.md")
	if err := os.WriteFile(bp, []byte("something else\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := st.Base(group, "MEMORY.md"); got != nil {
		t.Errorf("mismatched base used: %q", got)
	}
	st.Forget(group, "MEMORY.md")
	if _, ok := st.Hash(group, "MEMORY.md"); ok {
		t.Error("Forget left the record")
	}
	if _, err := os.Stat(bp); err == nil {
		t.Error("Forget left the base file")
	}

	// A torn or hand-mangled file is an empty record, which never deletes.
	if err := os.WriteFile(filepath.Join(dir, ".state", stateFileName), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, ok := LoadSyncState(dir).Hash(group, "topic.md"); ok {
		t.Error("corrupt state should load empty")
	}
	// A nil state (path mode) records and recalls nothing.
	var none *SyncState
	if _, ok := none.Hash(group, "x"); ok || none.Record(group, "x", nil) != nil || none.Save() != nil {
		t.Error("nil SyncState should be inert")
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

// The recorded directory is gone and the only directory the name decodes to now
// is a different one (src/my-app moved away, src/my.app appeared). Having no
// origin says nothing about the project that used to be there, so only a real
// origin may re-key the folder.
func TestRemoteKey_StickyKeyIgnoresAForeignCandidateWithoutOrigin(t *testing.T) {
	for _, tc := range []struct {
		name   string
		origin string // of the new candidate; "" means a plain directory
		want   string
		dirNow bool // the cache now records the new candidate
	}{
		{"plain directory", "", "github.com/acme/app", false},
		{"checkout without origin", "-", "github.com/acme/app", false},
		{"checkout with another origin", "git@github.com:acme/other.git", "github.com/acme/other", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gitEnv(t)
			shared := t.TempDir()
			a := newMachine(t, "alice", shared)
			old := mkRepo(t, filepath.Join(a.home, "src", "my-app"), "git@github.com:acme/app.git")
			hash := a.hash(old)
			a.write(old, "a.md", "a\n")
			m, err := NewRemoteMap(shared, a.claude)
			if err != nil {
				t.Fatal(err)
			}
			if got := m.Key(hash); got != "github.com/acme/app" {
				t.Fatalf("initial key = %q", got)
			}
			if err := os.RemoveAll(old); err != nil {
				t.Fatal(err)
			}
			repl := filepath.Join(a.home, "src", "my.app")
			switch tc.origin {
			case "":
				mkdirs(t, a.home, "src/my.app")
			case "-":
				mkRepo(t, repl, "")
			default:
				mkRepo(t, repl, tc.origin)
			}
			m.Refresh()
			if got := m.Key(hash); got != tc.want {
				t.Errorf("key = %q, want %q", got, tc.want)
			}
			c, err := LoadProjectCache(shared)
			if err != nil {
				t.Fatal(err)
			}
			wantDir := old
			if tc.dirNow {
				wantDir = repl
			}
			if c.Projects[hash].Dir != wantDir {
				t.Errorf("cached dir = %q, want %q", c.Projects[hash].Dir, wantDir)
			}
		})
	}
}

// A recorded dir that is missing (an unmounted drive, a moved checkout) is kept
// through a spell where its name decodes to several directories: it is only
// ever used after a stat, and clearing it would throw away a hand-pinned dir.
func TestRemoteKey_AmbiguousDecodeKeepsTheRecordedDir(t *testing.T) {
	gitEnv(t)
	shared := t.TempDir()
	a := newMachine(t, "alice", shared)
	old := mkRepo(t, filepath.Join(a.home, "src", "my-app"), "git@github.com:acme/one.git")
	hash := a.hash(old)
	a.write(old, "a.md", "a\n")
	m, err := NewRemoteMap(shared, a.claude)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(old); err != nil {
		t.Fatal(err)
	}
	mkRepo(t, filepath.Join(a.home, "src", "my.app"), "git@github.com:acme/two.git")
	mkRepo(t, filepath.Join(a.home, "src", "my", "app"), "git@github.com:acme/three.git")
	m.Refresh()
	if got := m.Key(hash); got != "github.com/acme/one" {
		t.Errorf("key = %q, want the remembered one", got)
	}
	c, err := LoadProjectCache(shared)
	if err != nil {
		t.Fatal(err)
	}
	e := c.Projects[hash]
	if e.Dir != old || len(e.Candidates) != 2 {
		t.Errorf("want the old dir kept and 2 candidates, got %+v", e)
	}
}

func TestGitOrigin(t *testing.T) {
	gitEnv(t)
	// Messages must not depend on the user's language: gitOrigin has to ask
	// for the C locale itself.
	t.Setenv("LC_ALL", "de_DE.UTF-8")
	t.Setenv("LANGUAGE", "de_DE")
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

// gitOrigin recognises "not a repository" by git's English message, so it must
// run git in the C locale. A stand-in git that only speaks English when asked
// to proves it does.
func TestGitOrigin_RunsGitInTheCLocale(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stand-in for git")
	}
	bin := t.TempDir()
	script := `#!/bin/sh
if [ "$LC_ALL" = C ] && [ "$LANGUAGE" = C ]; then
  echo "fatal: not a git repository (or any of the parent directories): .git" >&2
else
  echo "fatal: kein Git-Repository (oder irgendein Elternverzeichnis): .git" >&2
fi
exit 128
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("LC_ALL", "de_DE.UTF-8")
	t.Setenv("LANGUAGE", "de_DE")
	url, ok := gitOrigin(t.TempDir())
	if url != "" || !ok {
		t.Errorf("gitOrigin = %q,%v, want \"\",true (not a repository)", url, ok)
	}
}

func TestClassifyGitFailure(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{errors.New("git rev-parse: exit status 128: fatal: detected dubious ownership in repository at '/x'"), "dubious ownership"},
		{fmt.Errorf("git rev-parse: %w", &exec.Error{Name: "git", Err: exec.ErrNotFound}), "git not found"},
		{errors.New("git rev-parse: exit status 1: boom"), "other"},
	}
	for _, c := range cases {
		if got := classifyGitFailure(c.err); got != c.want {
			t.Errorf("classifyGitFailure(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// When git cannot answer and there is no remembered key, say so once per
// folder, naming the Claude folder and the class of failure but not the path.
func TestRemoteKey_LogsOnceWhenGitCannotAnswerAndNothingIsRemembered(t *testing.T) {
	gitEnv(t)
	shared := t.TempDir()
	a := newMachine(t, "alice", shared)
	app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	a.write(app, "a.md", "a\n")
	hash := a.hash(app)

	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	m, err := NewRemoteMap(shared, a.claude)
	if err != nil {
		t.Fatal(err)
	}
	// A cache that knows the directory but no key, as after a first run where
	// git was already failing.
	m.cache = &ProjectCache{Version: 1, Projects: map[string]ProjectEntry{hash: {Dir: app}}}
	m.Lookup = func(string) (string, bool) { return "", false }
	m.Why = func(string) string { return "dubious ownership" }
	logs.Reset()
	m.Refresh()
	m.Refresh()
	out := logs.String()
	if n := strings.Count(out, hash); n != 1 {
		t.Fatalf("want one log line naming %s, got %d in %q", hash, n, out)
	}
	if !strings.Contains(out, "dubious ownership") {
		t.Errorf("the reason class is missing: %q", out)
	}
	if strings.Contains(out, a.home) {
		t.Errorf("the log leaks the project path: %q", out)
	}

	// With a remembered key there is nothing to complain about.
	m.cache.Projects[hash] = ProjectEntry{Dir: app, LastKey: "github.com/acme/app"}
	m.warned = map[string]bool{}
	m.failed = map[string]bool{}
	logs.Reset()
	m.Refresh()
	if logs.Len() != 0 {
		t.Errorf("logged although the key is remembered: %q", logs.String())
	}
}

// A projects/<hash> folder that came from another PC's push has no project
// folder here: it must not be resolved or cached.
func TestRemoteKey_ForeignHashIsNotResolvedOrCached(t *testing.T) {
	gitEnv(t)
	shared := t.TempDir()
	a := newMachine(t, "alice", shared)
	app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	a.write(app, "a.md", "a\n")
	a.sync()

	roots, err := NewRoots(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	roots.Remote.Lookup = func(dir string) (string, bool) {
		t.Errorf("Lookup(%q) for a folder that is not under this PC's projects", dir)
		return "", false
	}
	if got := roots.Remote.Key("-home-bob-src-app"); got != "" {
		t.Errorf("Key = %q, want the path key", got)
	}
	if got := roots.Remote.Members(""); len(got) != 0 {
		t.Errorf("foreign hash became a member: %v", got)
	}
	c, err := LoadProjectCache(shared)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Projects["-home-bob-src-app"]; ok {
		t.Error("foreign hash written to the cache")
	}
	// propagateChanges resolves keys for pulled projects/<hash>/ paths too.
	if got := propagationTargets(roots, "projects/-home-bob-src-app/memory/x.md"); len(got) != 1 {
		t.Errorf("a foreign path-keyed file still propagates by its own name, got %v", got)
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

// After switching to remote mode, a folder's old projects/<hash>/memory copy
// stays in the repo and stops being updated; the daemon says so at startup.
func TestRoots_LogSupersededMirrorCopies(t *testing.T) {
	gitEnv(t)
	shared := t.TempDir()
	a := newMachine(t, "alice", shared)
	app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	fresh := mkRepo(t, filepath.Join(a.home, "src", "fresh"), "git@github.com:acme/fresh.git")
	plain := mkdirs(t, a.home, "notes")[0]
	for _, d := range []string{app, fresh, plain} {
		a.write(d, "n.md", "x\n")
	}
	// Left over from path mode: app and the plain folder have an old copy.
	for _, d := range []string{app, plain} {
		if err := os.MkdirAll(filepath.Join(shared, "projects", a.hash(d), "memory"), 0700); err != nil {
			t.Fatal(err)
		}
	}

	roots, err := NewRoots(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	roots.LogSuperseded()

	out := logs.String()
	if n := strings.Count(strings.TrimSpace(out), "\n") + 1; n != 1 || !strings.Contains(out, a.hash(app)) {
		t.Errorf("want one line for the switched folder, got %d in %q", n, out)
	}
	if !strings.Contains(out, "no longer updated") || !strings.Contains(out, "github.com/acme/app") {
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

// A hand-edited cache saved with a UTF-8 BOM (PowerShell 5 Set-Content
// -Encoding UTF8, older Notepad) is valid JSON to the user and must be read,
// not set aside.
func TestLoadProjectCache_StripsUTF8BOM(t *testing.T) {
	dir := t.TempDir()
	p := ProjectCachePath(dir)
	_ = os.MkdirAll(filepath.Dir(p), 0700)
	if err := os.WriteFile(p, []byte("\xef\xbb\xbf{\"version\":1,\"projects\":{\"-x\":{\"key\":\"path\"}}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadProjectCache(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Projects["-x"].Key != "path" {
		t.Errorf("hand edit with a BOM discarded; cache = %+v", c.Projects)
	}
}

func TestLoadProjectCache_UnknownVersionIsSetAside(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		aside      bool
	}{
		{"newer version", `{"version":2,"projects":{"-x":{"key":"path"}}}`, true},
		{"negative version", `{"version":-1,"projects":{}}`, true},
		{"current version", `{"version":1,"projects":{"-x":{"key":"path"}}}`, false},
		// A hand-written file is unlikely to carry the field.
		{"no version", `{"projects":{"-x":{"key":"path"}}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := ProjectCachePath(dir)
			if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			c, err := LoadProjectCache(dir)
			if err != nil {
				t.Fatal(err)
			}
			matches, _ := filepath.Glob(p + ".corrupt-*")
			if tc.aside {
				if len(matches) != 1 || len(c.Projects) != 0 {
					t.Errorf("want the file set aside and an empty cache: %v %v", matches, c.Projects)
				}
				return
			}
			if len(matches) != 0 || c.Projects["-x"].Key != "path" {
				t.Errorf("want the file used: %v %v", matches, c.Projects)
			}
		})
	}
}

// A cache that cannot be read for a reason other than not existing (a
// directory in its place, no permission) is not the daemon's to replace: it
// carries on with an empty in-memory cache and never writes the file.
func TestLoadProjectCache_UnreadableIsNotFatalAndNotOverwritten(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	t.Run("directory in place of the file", func(t *testing.T) {
		dir := t.TempDir()
		p := ProjectCachePath(dir)
		if err := os.MkdirAll(filepath.Join(p, "keep"), 0700); err != nil {
			t.Fatal(err)
		}
		c, err := LoadProjectCache(dir)
		if err != nil {
			t.Fatalf("a read error must not be fatal: %v", err)
		}
		c.Projects["-x"] = ProjectEntry{Key: "path"}
		if err := c.Save(dir); err != nil {
			t.Errorf("Save = %v, want a quiet no-op", err)
		}
		if info, err := os.Stat(p); err != nil || !info.IsDir() {
			t.Errorf("the unreadable path was replaced: %v %v", info, err)
		}
	})

	t.Run("permission denied", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Getuid() == 0 {
			t.Skip("cannot make a file unreadable here")
		}
		dir := t.TempDir()
		p := ProjectCachePath(dir)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		const body = `{"version":1,"projects":{"-x":{"key":"path"}}}`
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0); err != nil {
			t.Fatal(err)
		}
		logs.Reset()
		c, err := LoadProjectCache(dir)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(logs.String(), "projects.json") {
			t.Errorf("the read error should be logged: %q", logs.String())
		}
		if err := c.Save(dir); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0600); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(p); string(b) != body {
			t.Errorf("the file was overwritten: %q", b)
		}
	})
}

func TestRemoteKey_UnreadableCacheDoesNotStopTheDaemon(t *testing.T) {
	gitEnv(t)
	shared := t.TempDir()
	a := newMachine(t, "alice", shared)
	app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	a.write(app, "a.md", "a\n")
	if err := os.MkdirAll(filepath.Join(ProjectCachePath(shared), "dir"), 0700); err != nil {
		t.Fatal(err)
	}
	a.sync() // fails the test if NewRoots errors
	if _, err := os.Stat(filepath.Join(shared, "remotes", "github.com", "acme", "app", "memory", "a.md")); err != nil {
		t.Errorf("keys still resolve without the cache file: %v", err)
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

// ---- a member leaving its group ----

// repoFiles lists the files in a repo-side memory dir.
func repoFiles(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		if !e.IsDir() && !strings.Contains(e.Name(), ".from-remote-") {
			out = append(out, e.Name())
		}
	}
	return out
}

// Losing a clone's whole memory/ dir, or its whole project folder, is that
// clone leaving the group. It is not a delete of each file in it: the shared
// notes stay in the repo and in the other clone.
func TestRemoteKey_RemovedMemoryDirOrFolderLeavesTheGroup(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remove func(a *machine, c string) error
	}{
		{"memory dir", func(a *machine, c string) error {
			return os.RemoveAll(filepath.Join(a.claude, a.hash(c), "memory"))
		}},
		{"project folder", func(a *machine, c string) error {
			return os.RemoveAll(filepath.Join(a.claude, a.hash(c)))
		}},
	} {
		for _, clones := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s with %d clone(s)", tc.name, clones), func(t *testing.T) {
				a, c1, c2, repoDir := twoClones(t)
				if clones == 1 {
					if err := os.RemoveAll(filepath.Join(a.claude, a.hash(c2))); err != nil {
						t.Fatal(err)
					}
				}
				for _, f := range []string{"one.md", "two.md", "MEMORY.md"} {
					a.write(c1, f, "# "+f+"\n")
				}
				a.sync()
				if err := tc.remove(a, c1); err != nil {
					t.Fatal(err)
				}
				a.sync()
				if got := repoFiles(t, repoDir); len(got) != 3 {
					t.Errorf("shared notes wiped from the repo: %v", got)
				}
				if clones == 2 {
					for _, f := range []string{"one.md", "two.md", "MEMORY.md"} {
						if _, ok := a.read(c2, f); !ok {
							t.Errorf("%s was deleted from the other clone", f)
						}
					}
				}
			})
		}
	}
}

// The loop sees an `rm -rf` of a clone's memory/ as a burst of Remove events.
// None of them may reach the repo, whether the dir still exists when an event
// is handled or not, and the other clone keeps its notes.
func TestLoop_RemovingMemoryDirWhileRunningKeepsSharedNotes(t *testing.T) {
	a, c1, c2, repoDir := twoClones(t)
	origin := withOrigin(t, a.cfg.SyncDir)
	_ = origin
	var files []string
	for i := 0; i < 40; i++ {
		f := fmt.Sprintf("note_%02d.md", i)
		files = append(files, f)
		a.write(c1, f, f+"\n")
	}
	a.sync()
	if err := gitwt.New(a.cfg.SyncDir).Push("main"); err != nil {
		t.Fatal(err)
	}

	a.cfg.DebounceMs = 100
	flushed := make(chan struct{}, 64)
	l := &Loop{Cfg: a.cfg, Branch: "main", Hostname: "alice", OnFlush: func(bool, error) { flushed <- struct{}{} }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()
	var once gosync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("loop did not stop")
			}
		})
	}
	defer stop()
	waitFlush := func(what string) {
		t.Helper()
		select {
		case <-flushed:
		case <-time.After(20 * time.Second):
			t.Fatalf("no flush %s", what)
		}
	}
	waitFlush("at startup")
	time.Sleep(200 * time.Millisecond)
	for len(flushed) > 0 {
		<-flushed
	}

	if err := os.RemoveAll(filepath.Join(a.claude, a.hash(c1), "memory")); err != nil {
		t.Fatal(err)
	}
	waitFlush("after the rm -rf")
	// One more debounce window, in case events straggled in.
	time.Sleep(400 * time.Millisecond)
	stop()

	if got := repoFiles(t, repoDir); len(got) != len(files) {
		t.Errorf("rm -rf of one clone's memory/ wiped the shared notes: %d of %d left", len(got), len(files))
	}
	for _, f := range files {
		if _, ok := a.read(c2, f); !ok {
			t.Errorf("%s deleted from the other clone", f)
		}
	}
	if out, err := gitwt.New(a.cfg.SyncDir).Run("log", "--diff-filter=D", "--name-only", "--format=", "-5"); err != nil || strings.TrimSpace(out) != "" {
		t.Errorf("a commit deleted shared notes: %q %v", out, err)
	}
}

// A single deleted file is still a delete: the Remove event asks for a
// reconcile instead of touching the repo, and the reconcile applies it.
func TestHandleEvent_RemoveInRemoteModeDefersToReconcile(t *testing.T) {
	a, c1, c2, repoDir := twoClones(t)
	a.write(c1, "topic.md", "x\n")
	a.write(c1, "keep.md", "k\n")
	a.sync()
	roots, err := NewRoots(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	a.remove(c1, "topic.md")
	l := &Loop{Cfg: a.cfg}
	ev := fsnotify.Event{Name: filepath.Join(a.claude, a.hash(c1), "memory", "topic.md"), Op: fsnotify.Remove}
	handled, err := l.handleEvent(nil, roots, ev)
	if err != nil || !handled || !l.needReconcile {
		t.Fatalf("handled=%v needReconcile=%v err=%v", handled, l.needReconcile, err)
	}
	if _, err := os.Stat(filepath.Join(repoDir, "topic.md")); err != nil {
		t.Error("the event itself must not delete from the repo")
	}
	// The dir going away is not a file delete at all.
	if err := os.RemoveAll(filepath.Join(a.claude, a.hash(c2), "memory")); err != nil {
		t.Fatal(err)
	}
	l = &Loop{Cfg: a.cfg}
	ev = fsnotify.Event{Name: filepath.Join(a.claude, a.hash(c2), "memory", "keep.md"), Op: fsnotify.Remove}
	if handled, err := l.handleEvent(nil, roots, ev); err != nil || handled {
		t.Errorf("a Remove under a vanished memory dir: handled=%v err=%v", handled, err)
	}
	if len(l.removed) != 0 {
		t.Errorf("a vanished memory dir should record no deletes: %v", l.removed)
	}
	dirEv := fsnotify.Event{Name: filepath.Join(a.claude, a.hash(c2), "memory"), Op: fsnotify.Remove}
	if handled, err := l.handleEvent(nil, roots, dirEv); err != nil || handled {
		t.Errorf("a Remove of the dir itself: handled=%v err=%v", handled, err)
	}
	a.sync()
	if _, err := os.Stat(filepath.Join(repoDir, "keep.md")); err != nil {
		t.Error("keep.md lost")
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
