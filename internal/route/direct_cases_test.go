//go:build unix

package route

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newRepo(t *testing.T) (dir, head string) {
	t.Helper()
	gitEnv(t)
	dir = filepath.Join(t.TempDir(), "r")
	initRepo(t, dir, "https://example.com/o/r.git")
	h, err := gitOut(dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return dir, h
}

func TestReadCoreConfig(t *testing.T) {
	cases := []struct {
		name, body string
		bare       bool
		wt         string
		ok         bool
	}{
		{"plain", "[core]\n\tbare = false\n", false, "", true},
		{"bare true", "[core]\n\tbare = true\n", true, "", true},
		{"bare yes spelled on", "[core]\nbare = on\n", true, "", true},
		{"bare 0", "[core]\nbare = 0\n", false, "", true},
		{"worktree", "[core]\nworktree = ../w\n", false, "../w", true},
		{"section case and comments", "# c\n; c\n[CORE]\n\n  Bare = false\n", false, "", true},
		{"unrecognised bare spelling is treated as bare", "[core]\nbare = FALSE\n", true, "", true},
		{"other section ignored", "[remote \"o\"]\nbare = true\nworktree = x\n", false, "", true},
		{"include", "[include]\npath = x\n", false, "", false},
		{"includeIf", "[includeIf \"gitdir:/x\"]\npath = x\n", false, "", false},
		{"unclosed section", "[core\nbare = true\n", false, "", false},
		{"empty section", "[]\n", false, "", false},
		{"key on section line", "[core] bare = true\n", false, "", false},
		{"comment on section line", "[core] # note\nbare = true\n", true, "", true},
		{"quoted worktree", "[core]\nworktree = \"x\"\n", false, "", false},
		{"escaped worktree", "[core]\nworktree = a\\b\n", false, "", false},
	}
	for _, c := range cases {
		p := filepath.Join(t.TempDir(), "config")
		writeFile(t, p, c.body)
		bare, wt, ok := readCoreConfig(p)
		if bare != c.bare || wt != c.wt || ok != c.ok {
			t.Errorf("%s: got (%v, %q, %v), want (%v, %q, %v)", c.name, bare, wt, ok, c.bare, c.wt, c.ok)
		}
	}
	if _, _, ok := readCoreConfig(filepath.Join(t.TempDir(), "absent")); ok {
		t.Error("a missing config was read as ok")
	}
	big := filepath.Join(t.TempDir(), "big")
	writeFile(t, big, "[core]\n#"+strings.Repeat("x", 1<<20)+"\n")
	if _, _, ok := readCoreConfig(big); ok {
		t.Error("an oversize config was read as ok")
	}
}

func TestSafeRef(t *testing.T) {
	good := []string{"refs/heads/main", "refs/heads/feat/x", "refs/worktree/a"}
	bad := []string{"heads/main", "refs/heads/../x", "refs//x", "refs/heads/", "refs/heads/a.lock",
		"refs/heads/a\\b", "refs/heads/a\x00b", "refs/heads/a\tb", "refs/heads/a\x7fb", ""}
	for _, r := range good {
		if !safeRef(r) {
			t.Errorf("safeRef(%q) = false", r)
		}
	}
	for _, r := range bad {
		if safeRef(r) {
			t.Errorf("safeRef(%q) = true", r)
		}
	}
}

func TestValidHEADFile(t *testing.T) {
	sha := strings.Repeat("ab", 20)
	for body, want := range map[string]bool{
		"ref: refs/heads/main\n": true,
		sha + "\n":               true,
		"ref: main\n":            false,
		"garbage\n":              false,
		sha[:39] + "\n":          false,
		"":                       false,
	} {
		d := t.TempDir()
		writeFile(t, filepath.Join(d, "HEAD"), body)
		if got := validHEADFile(d); got != want {
			t.Errorf("validHEADFile(%q) = %v, want %v", body, got, want)
		}
	}
	if validHEADFile(t.TempDir()) {
		t.Error("no HEAD file counted as valid")
	}
	d := t.TempDir()
	writeFile(t, filepath.Join(d, "HEAD"), "ref: refs/heads/"+strings.Repeat("a", 5000))
	if validHEADFile(d) {
		t.Error("an oversize HEAD counted as valid")
	}
}

func TestLocateRepoAnswers(t *testing.T) {
	repo, _ := newRepo(t)
	sub := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}

	loc, k := locateRepo(sub)
	if k != directYes || loc.worktree != repo || loc.gitDir != filepath.Join(repo, ".git") || loc.commonDir != loc.gitDir {
		t.Errorf("subdir of a checkout: %+v %v", loc, k)
	}
	if _, k := locateRepo("relative/dir"); k != directDoubt {
		t.Errorf("relative path: %v", k)
	}
	if _, k := locateRepo(filepath.Join(repo, ".git", "refs")); k != directDoubt {
		t.Errorf("inside a git dir: %v", k)
	}
	if _, k := locateRepo(filepath.Join(repo, "nope")); k != directNo {
		t.Errorf("missing dir: %v", k)
	}
	writeFile(t, filepath.Join(repo, "file"), "x")
	if _, k := locateRepo(filepath.Join(repo, "file")); k != directDoubt {
		t.Errorf("a file: %v", k)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	if _, k := locateRepo(link); k != directDoubt {
		t.Errorf("a symlinked dir: %v", k)
	}
	if k := directInside(sub); k != directYes {
		t.Errorf("directInside = %v", k)
	}
}

func TestLocateRepoBareAndSymlinkedGitAreDoubt(t *testing.T) {
	gitEnv(t)
	bare := filepath.Join(t.TempDir(), "b.git")
	gitIn(t, t.TempDir(), "init", "-q", "--bare", "-b", "main", bare)
	if _, k := locateRepo(bare); k != directDoubt {
		t.Errorf("a bare repo: %v", k)
	}

	repo, _ := newRepo(t)
	moved := filepath.Join(t.TempDir(), "gitdir")
	if err := os.Rename(filepath.Join(repo, ".git"), moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, filepath.Join(repo, ".git")); err != nil {
		t.Fatal(err)
	}
	if _, k := locateRepo(repo); k != directDoubt {
		t.Errorf("a symlinked .git: %v", k)
	}
}

// Each odd shape of .git that a direct read must hand to git.
func TestOpenGitPathDoubts(t *testing.T) {
	mutate := map[string]func(t *testing.T, repo string){
		"HEAD not a ref": func(t *testing.T, repo string) {
			writeFile(t, filepath.Join(repo, ".git", "HEAD"), "garbage\n")
		},
		"objects is a symlink": func(t *testing.T, repo string) {
			o := filepath.Join(repo, ".git", "objects")
			if err := os.Rename(o, o+".real"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(o+".real", o); err != nil {
				t.Fatal(err)
			}
		},
		"refs missing": func(t *testing.T, repo string) {
			if err := os.RemoveAll(filepath.Join(repo, ".git", "refs")); err != nil {
				t.Fatal(err)
			}
		},
		"config.worktree": func(t *testing.T, repo string) {
			writeFile(t, filepath.Join(repo, ".git", "config.worktree"), "[core]\n")
		},
		"core.bare true": func(t *testing.T, repo string) {
			gitIn(t, repo, "config", "core.bare", "true")
		},
		"config with an include": func(t *testing.T, repo string) {
			gitIn(t, repo, "config", "include.path", "/nonexistent")
		},
		"config unreadable": func(t *testing.T, repo string) {
			if err := os.Remove(filepath.Join(repo, ".git", "config")); err != nil {
				t.Fatal(err)
			}
		},
		"core.worktree elsewhere": func(t *testing.T, repo string) {
			gitIn(t, repo, "config", "core.worktree", t.TempDir())
		},
		"odd commondir": func(t *testing.T, repo string) {
			writeFile(t, filepath.Join(repo, ".git", "commondir"), "\n")
		},
		"commondir without objects": func(t *testing.T, repo string) {
			writeFile(t, filepath.Join(repo, ".git", "commondir"), t.TempDir()+"\n")
		},
	}
	for name, fn := range mutate {
		repo, _ := newRepo(t)
		fn(t, repo)
		if _, k := locateRepo(repo); k != directDoubt {
			t.Errorf("%s: kind %v, want doubt", name, k)
		}
	}

	// core.worktree naming the checkout itself is fine.
	repo, _ := newRepo(t)
	gitIn(t, repo, "config", "core.worktree", repo)
	if _, k := locateRepo(repo); k != directYes {
		t.Errorf("core.worktree = the checkout: %v, want yes", k)
	}
}

func TestOpenGitPathFileForms(t *testing.T) {
	repo, _ := newRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	gitIn(t, repo, "worktree", "add", "-q", "--detach", wt)

	loc, k := locateRepo(wt)
	realCommon, _ := filepath.EvalSymlinks(filepath.Join(repo, ".git")) // /tmp is a symlink on macOS
	gotCommon, _ := filepath.EvalSymlinks(loc.commonDir)
	if k != directYes || loc.worktree != wt || gotCommon != realCommon || loc.gitDir == loc.commonDir {
		t.Fatalf("a linked worktree: %+v %v", loc, k)
	}
	// The same through a relative gitdir: pointer.
	rel, err := filepath.Rel(wt, loc.gitDir)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt, ".git"), "gitdir: "+rel+"\n")
	if l2, k := locateRepo(wt); k != directYes || l2.gitDir != loc.gitDir {
		t.Errorf("relative gitdir pointer: %+v %v", l2, k)
	}
	for name, body := range map[string]string{
		"no prefix": "somewhere\n",
		"empty":     "gitdir: \n",
		"missing":   "gitdir: /nonexistent/x\n",
		"oversize":  "gitdir: " + strings.Repeat("a", 5000),
	} {
		writeFile(t, filepath.Join(wt, ".git"), body)
		if _, k := locateRepo(wt); k != directDoubt {
			t.Errorf("%s: kind %v, want doubt", name, k)
		}
	}
}

func TestDirectHEADForms(t *testing.T) {
	repo, head := newRepo(t)
	head = strings.TrimSpace(head)
	gd := filepath.Join(repo, ".git")

	if got, k := directHEAD(repo); k != directYes || got != head {
		t.Fatalf("loose ref: %q %v, want %q", got, k, head)
	}
	gitIn(t, repo, "pack-refs", "--all", "--prune")
	if got, k := directHEAD(repo); k != directYes || got != head {
		t.Fatalf("packed ref: %q %v", got, k)
	}
	// A packed entry with a bad object name is not trusted.
	packed := filepath.Join(gd, "packed-refs")
	good, _ := os.ReadFile(packed)
	writeFile(t, packed, "# pack-refs\nzzzz refs/heads/main\n")
	if _, k := directHEAD(repo); k != directDoubt {
		t.Errorf("bad packed entry: %v", k)
	}
	// Ref absent from packed-refs and loose: an unborn branch.
	writeFile(t, packed, string(good))
	writeFile(t, filepath.Join(gd, "HEAD"), "ref: refs/heads/unborn\n")
	if got, k := directHEAD(repo); k != directNo || got != "" {
		t.Errorf("unborn branch: %q %v", got, k)
	}
	// Detached.
	writeFile(t, filepath.Join(gd, "HEAD"), head+"\n")
	if got, k := directHEAD(repo); k != directYes || got != head {
		t.Errorf("detached: %q %v", got, k)
	}
	writeFile(t, filepath.Join(gd, "HEAD"), "ref: refs/heads/../x\n")
	if _, k := directHEAD(repo); k != directDoubt {
		t.Errorf("unsafe ref: %v", k)
	}
	// A loose ref that is not an object name (a symbolic ref) is left to git.
	writeFile(t, filepath.Join(gd, "HEAD"), "ref: refs/heads/sym\n")
	writeFile(t, filepath.Join(gd, "refs", "heads", "sym"), "ref: refs/heads/main\n")
	if _, k := directHEAD(repo); k != directDoubt {
		t.Errorf("symbolic loose ref: %v", k)
	}
	// Per-worktree ref namespaces live in the git dir.
	writeFile(t, filepath.Join(gd, "HEAD"), "ref: refs/bisect/bad\n")
	writeFile(t, filepath.Join(gd, "refs", "bisect", "bad"), head+"\n")
	if got, k := directHEAD(repo); k != directYes || got != head {
		t.Errorf("bisect ref: %q %v", got, k)
	}
	// Not a repo.
	if _, k := directHEAD(filepath.Join(repo, "nope")); k != directDoubt {
		t.Errorf("missing dir: %v", k)
	}
}

func TestDirectHEADLeavesNewRefStorageToGit(t *testing.T) {
	repo, _ := newRepo(t)
	gd := filepath.Join(repo, ".git")
	if err := os.Mkdir(filepath.Join(gd, "reftable"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, k := directHEAD(repo); k != directDoubt {
		t.Errorf("reftable dir: %v", k)
	}
	if err := os.Remove(filepath.Join(gd, "reftable")); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "config", "extensions.refStorage", "reftable")
	if _, k := directHEAD(repo); k != directDoubt {
		t.Errorf("refstorage in config: %v", k)
	}
}
