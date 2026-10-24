package sync

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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

// ---- helpers: real git checkouts ----

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

// A linked worktree shares its repository's auto-memory folder, so it is not
// a project of its own: gitOrigin reports no origin for it.
func TestGitOrigin_LinkedWorktreeHasNoOrigin(t *testing.T) {
	gitEnv(t)
	root := t.TempDir()
	main := mkRepo(t, filepath.Join(root, "app"), "git@github.com:acme/app.git")
	wt := filepath.Join(root, "app-feature")
	addWorktree(t, main, wt)
	if url, ok := gitOrigin(wt); url != "" || !ok {
		t.Errorf("worktree: gitOrigin = %q,%v, want \"\",true", url, ok)
	}
	if url, ok := gitOrigin(main); url != "git@github.com:acme/app.git" || !ok {
		t.Errorf("main checkout: gitOrigin = %q,%v", url, ok)
	}
}
