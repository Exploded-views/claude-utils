package route

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/proc"
)

// B9: route resolution answers "is this a checkout", the common dir and HEAD
// from the filesystem, and asks git only when it cannot tell. Every answer
// must equal what git says, so each test compares against real git.

// gitOnlyRemoteURL is remoteURL as it was before direct reads: three git calls.
func gitOnlyRemoteURL(dir string) (string, bool) {
	git := func(args ...string) (string, error) { return gitOut(dir, args...) }
	if in, err := git("rev-parse", "--is-inside-work-tree"); err != nil || in != "true" {
		return "", false
	}
	if _, err := git("rev-parse", "--git-common-dir"); err != nil {
		return "", false
	}
	u, err := git("remote", "get-url", "origin")
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", true
		}
		return "", false
	}
	return u, true
}

type directFixture struct {
	dirs  map[string]string // name -> directory
	names []string
}

func (f *directFixture) add(name, dir string) {
	f.dirs[name] = dir
	f.names = append(f.names, name)
}

func commitIn(t *testing.T, dir string) {
	t.Helper()
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "c")
}

func initRepo(t *testing.T, dir, remote string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "init", "-q", "-b", "main")
	gitIn(t, dir, "remote", "add", "origin", remote)
	commitIn(t, dir)
}

// newDirectFixture builds a tree of every kind of directory resolution meets.
// With the hard cases only (hard=true) the ones where a wrong direct read
// would differ from git: symlinked .git, unreadable and corrupt HEAD.
func newDirectFixture(t *testing.T) *directFixture {
	t.Helper()
	gitEnv(t)
	root := t.TempDir()
	f := &directFixture{dirs: map[string]string{}}

	plain := filepath.Join(root, "plain")
	initRepo(t, plain, "git@github.com:me/plain.git")
	f.add("plain", plain)
	if err := os.MkdirAll(filepath.Join(plain, "sub", "deep"), 0o700); err != nil {
		t.Fatal(err)
	}
	f.add("subdir", filepath.Join(plain, "sub", "deep"))

	wt := filepath.Join(root, "linked")
	gitIn(t, plain, "worktree", "add", "-q", "-b", "wtb", wt)
	f.add("linked-worktree", wt)

	detached := filepath.Join(root, "detached")
	initRepo(t, detached, "git@github.com:me/detached.git")
	gitIn(t, detached, "checkout", "-q", "--detach")
	f.add("detached", detached)

	packed := filepath.Join(root, "packed")
	initRepo(t, packed, "git@github.com:me/packed.git")
	gitIn(t, packed, "pack-refs", "--all", "--prune")
	if _, err := os.Stat(filepath.Join(packed, ".git", "refs", "heads", "main")); err == nil {
		t.Fatal("loose ref still there after pack-refs --prune")
	}
	f.add("packed-only", packed)

	unborn := filepath.Join(root, "unborn")
	if err := os.MkdirAll(unborn, 0o700); err != nil {
		t.Fatal(err)
	}
	gitIn(t, unborn, "init", "-q", "-b", "main")
	gitIn(t, unborn, "remote", "add", "origin", "git@github.com:me/unborn.git")
	f.add("unborn", unborn)

	noremote := filepath.Join(root, "noremote")
	if err := os.MkdirAll(noremote, 0o700); err != nil {
		t.Fatal(err)
	}
	gitIn(t, noremote, "init", "-q", "-b", "main")
	commitIn(t, noremote)
	f.add("no-origin", noremote)

	super := filepath.Join(root, "super")
	initRepo(t, super, "git@github.com:me/super.git")
	gitIn(t, super, "-c", "protocol.file.allow=always", "submodule", "add", "-q", plain, "mod")
	f.add("submodule", filepath.Join(super, "mod"))

	real := filepath.Join(root, "real")
	initRepo(t, real, "git@github.com:me/real.git")
	sym := filepath.Join(root, "sym")
	if err := os.MkdirAll(sym, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(real, ".git"), filepath.Join(sym, ".git")); err == nil {
		f.add("symlinked-.git", sym)
	}

	// A broken .git inside a good checkout: git ignores it and uses the outer
	// one, so the direct read must not answer "not a repo".
	outer := filepath.Join(root, "outer")
	initRepo(t, outer, "git@github.com:me/outer.git")
	corrupt := filepath.Join(outer, "corrupt")
	initRepo(t, corrupt, "git@github.com:me/corrupt.git")
	if err := os.WriteFile(filepath.Join(corrupt, ".git", "HEAD"), []byte("\x00\xff garbage \x01"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.add("corrupt-HEAD", corrupt)
	if runtime.GOOS != "windows" && os.Getuid() != 0 {
		unread := filepath.Join(outer, "unreadable")
		initRepo(t, unread, "git@github.com:me/unreadable.git")
		head := filepath.Join(unread, ".git", "HEAD")
		if err := os.Chmod(head, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(head, 0o600) })
		f.add("unreadable-HEAD", unread)
	}

	// A symlink above the checkout root, same volume: git walks the physical
	// path and finds the checkout, a walk of the path as written does not.
	link := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Join(plain, "sub"), link); err == nil {
		f.add("symlinked-ancestor", filepath.Join(link, "deep"))
	}

	nonrepo := filepath.Join(root, "nonrepo")
	if err := os.MkdirAll(nonrepo, 0o700); err != nil {
		t.Fatal(err)
	}
	f.add("non-repo", nonrepo)
	f.add("missing", filepath.Join(root, "no", "such", "dir"))
	return f
}

// rootsOf is what the folder's roots lookup answers with the given git: nil Git
// is the default (direct reads); gitOut is the git-only path.
func rootsOf(dir string, git func(string, ...string) (string, error)) ([]string, bool) {
	r := New(File{}, func(string) bool { return true })
	r.Git = git
	return r.defaultRoots(escape(dir))
}

func TestDirectReadsEqualGit(t *testing.T) {
	f := newDirectFixture(t)
	for _, name := range f.names {
		dir := f.dirs[name]
		t.Run(name, func(t *testing.T) {
			wantU, wantOK := gitOnlyRemoteURL(dir)
			gotU, gotOK := remoteURL(dir)
			if gotU != wantU || gotOK != wantOK {
				t.Errorf("remoteURL = (%q, %v), git says (%q, %v)", gotU, gotOK, wantU, wantOK)
			}
			wantR, wantRok := rootsOf(dir, gitOut)
			gotR, gotRok := rootsOf(dir, nil)
			if !slices.Equal(gotR, wantR) || gotRok != wantRok {
				t.Errorf("roots = (%v, %v), git says (%v, %v)", gotR, gotRok, wantR, wantRok)
			}
		})
	}
}

// Each case git answers "yes, a checkout" for must not become "no" because the
// direct read doubted: a symlinked .git, and a broken .git inside a good one.
func TestDirectDoubtAsksGit(t *testing.T) {
	f := newDirectFixture(t)
	for _, name := range []string{"symlinked-.git", "corrupt-HEAD", "unreadable-HEAD", "submodule", "linked-worktree", "symlinked-ancestor"} {
		dir, ok := f.dirs[name]
		if !ok {
			continue
		}
		t.Run(name, func(t *testing.T) {
			want, wantOK := gitOnlyRemoteURL(dir)
			if !wantOK || want == "" {
				t.Fatalf("fixture: git does not say %s is a checkout with an origin", name)
			}
			if got, ok := remoteURL(dir); !ok || got != want {
				t.Errorf("remoteURL = (%q, %v), want (%q, true)", got, ok, want)
			}
		})
	}
}

type spawnLog struct {
	mu   sync.Mutex
	subs []string
}

func countSpawns(t *testing.T) *spawnLog {
	t.Helper()
	l := &spawnLog{}
	restore := proc.SetSpawnHook(func(name string, args []string) {
		l.mu.Lock()
		l.subs = append(l.subs, strings.Join(args, " "))
		l.mu.Unlock()
	})
	t.Cleanup(restore)
	return l
}

func (l *spawnLog) n() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.subs)
}

// The counted gate in the ordinary suite: a non-repo costs no git process, a
// checkout at most one (remote get-url), whatever kind of checkout it is.
func TestResolutionSpawnCounts(t *testing.T) {
	f := newDirectFixture(t)
	l := countSpawns(t)
	for name, want := range map[string]int{"non-repo": 1, "missing": 0} {
		before := l.n()
		if _, ok := remoteURL(f.dirs[name]); ok {
			t.Errorf("%s: remoteURL ok", name)
		}
		if d := l.n() - before; d != want {
			t.Errorf("%s: %d git spawns, want %d (%v)", name, d, want, l.subs[before:])
		}
	}
	for _, name := range []string{"plain", "subdir", "linked-worktree", "detached", "packed-only", "unborn", "no-origin", "submodule"} {
		before := l.n()
		remoteURL(f.dirs[name])
		if name == "unborn" { // the fixture's checkouts have no commit: HEAD is asked too
			rootsOf(f.dirs[name], nil)
		}
		if d := l.n() - before; d > 1 {
			t.Errorf("%s: %d git spawns, want at most 1: %v", name, d, l.subs[before:])
		}
	}
}

// All the directories at once, in a mixed tree: the answers do not depend on
// the order or on how many are resolved together.
func TestDirectReadsEqualGitMixedTree(t *testing.T) {
	f := newDirectFixture(t)
	names := slices.Clone(f.names)
	sort.Strings(names)
	type ans struct {
		u   string
		ok  bool
		r   []string
		rok bool
	}
	want := map[string]ans{}
	for _, n := range names {
		u, ok := gitOnlyRemoteURL(f.dirs[n])
		r, rok := rootsOf(f.dirs[n], gitOut)
		want[n] = ans{u, ok, r, rok}
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, n := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u, ok := remoteURL(f.dirs[n])
			r, rok := rootsOf(f.dirs[n], nil)
			w := want[n]
			if u != w.u || ok != w.ok || !slices.Equal(r, w.r) || rok != w.rok {
				mu.Lock()
				t.Errorf("%s: direct (%q, %v, %v, %v), git (%q, %v, %v, %v)", n, u, ok, r, rok, w.u, w.ok, w.r, w.rok)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
}

// A directory that is gone costs no git process, however many there are.
func TestMissingDirsCostNoGitSpawn(t *testing.T) {
	gitEnv(t)
	root := t.TempDir()
	l := countSpawns(t)
	for i := 0; i < 50; i++ {
		if _, ok := remoteURL(filepath.Join(root, "gone", strings.Repeat("x", i%5+1))); ok {
			t.Fatal("remoteURL ok for a missing dir")
		}
	}
	if n := l.n(); n != 0 {
		t.Fatalf("%d git spawns for missing dirs, want 0: %v", n, l.subs)
	}
}

// git's confirmation that a directory is no checkout is kept for the routes
// generation: any number of callers deciding the same folder cost one spawn,
// and a Replace or Refresh costs one more.
func TestNonRepoConfirmationIsAskedOncePerGeneration(t *testing.T) {
	gitEnv(t)
	dir := filepath.Join(t.TempDir(), "plainfolder")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	r := New(fileOf(Rule{Remote: "github.com/me/*", Repo: "projects"}), func(string) bool { return true })
	r.Logf = func(string, ...any) {}
	l := countSpawns(t)
	folder := escape(dir)
	decideMany := func() {
		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if d := r.Decide(folder); d.Repo != "" {
					t.Errorf("non-repo routed: %+v", d)
				}
			}()
		}
		wg.Wait()
		r.Warm([]string{folder, folder})
	}
	decideMany()
	if n := l.n(); n != 1 {
		t.Fatalf("%d spawns for one non-repo in one generation, want 1: %v", n, l.subs)
	}
	r.Refresh()
	decideMany()
	if n := l.n(); n != 2 {
		t.Fatalf("%d spawns after Refresh, want 2: %v", n, l.subs)
	}
	r.Replace(fileOf(Rule{Remote: "github.com/me/*", Repo: "projects"}))
	decideMany()
	if n := l.n(); n != 3 {
		t.Fatalf("%d spawns after Replace, want 3: %v", n, l.subs)
	}
}
