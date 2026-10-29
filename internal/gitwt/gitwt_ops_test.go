package gitwt

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func gitIdent(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
}

func TestIsRepoAndHasHeadFollowTheRepoLifecycle(t *testing.T) {
	gitIdent(t)
	r := testRepo(t)
	if !r.IsRepo() {
		t.Error("an initialised dir is not a repo")
	}
	if New(t.TempDir()).IsRepo() {
		t.Error("a plain dir counted as a repo")
	}
	if r.HasHead() {
		t.Error("an empty repo has a HEAD")
	}
	write(t, r, "a.md", []byte("a\n"))
	if err := r.AddAll(); err != nil {
		t.Fatal(err)
	}
	made, err := r.Commit("first")
	if err != nil || !made {
		t.Fatalf("Commit = %v, %v", made, err)
	}
	if !r.HasHead() {
		t.Error("no HEAD after a commit")
	}
	if br, err := r.CurrentBranch(); err != nil || br != "main" {
		t.Errorf("CurrentBranch = %q, %v", br, err)
	}
	// Nothing staged: Commit makes no commit and says so.
	if made, err := r.Commit("empty"); err != nil || made {
		t.Errorf("Commit with nothing staged = %v, %v", made, err)
	}
}

func TestHasStagedChangesDirtyPathsHeadFilesAndUnstageAll(t *testing.T) {
	gitIdent(t)
	r := testRepo(t)
	if files, err := r.HeadFiles(""); err != nil || len(files) != 0 {
		t.Fatalf("HeadFiles on an unborn HEAD = %v, %v", files, err)
	}
	write(t, r, "keep.md", []byte("k\n"))
	if err := os.MkdirAll(filepath.Join(r.Dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, r, "sub/in.md", []byte("i\n"))
	if has, err := r.HasStagedChanges(); err != nil || has {
		t.Fatalf("staged before add = %v, %v", has, err)
	}
	if err := r.AddAll(); err != nil {
		t.Fatal(err)
	}
	if has, err := r.HasStagedChanges(); err != nil || !has {
		t.Fatalf("staged after add = %v, %v", has, err)
	}
	if err := r.UnstageAll(); err != nil {
		t.Fatal(err)
	}
	if has, _ := r.HasStagedChanges(); has {
		t.Error("UnstageAll left staged changes")
	}
	if _, err := os.Stat(filepath.Join(r.Dir, "keep.md")); err != nil {
		t.Errorf("UnstageAll touched the work-tree: %v", err)
	}
	if err := r.AddAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("c"); err != nil {
		t.Fatal(err)
	}

	write(t, r, "sub/in.md", []byte("changed\n"))
	write(t, r, "new.md", []byte("n\n"))
	dirty, err := r.DirtyPaths("")
	sort.Strings(dirty)
	if err != nil || !reflect.DeepEqual(dirty, []string{"new.md", "sub/in.md"}) {
		t.Errorf("DirtyPaths(\"\") = %v, %v", dirty, err)
	}
	if dirty, err := r.DirtyPaths("sub"); err != nil || !reflect.DeepEqual(dirty, []string{"sub/in.md"}) {
		t.Errorf("DirtyPaths(sub) = %v, %v", dirty, err)
	}
	all, err := r.HeadFiles("")
	if err != nil || !all["keep.md"] || !all["sub/in.md"] || all["new.md"] || len(all) != 2 {
		t.Errorf("HeadFiles(\"\") = %v, %v", all, err)
	}
	sub, err := r.HeadFiles("sub")
	if err != nil || !sub["sub/in.md"] || len(sub) != 1 {
		t.Errorf("HeadFiles(sub) = %v, %v", sub, err)
	}
}

func TestIsAncestorConfigSetAndRunClean(t *testing.T) {
	gitIdent(t)
	r := testRepo(t)
	write(t, r, "a", []byte("1\n"))
	_ = r.AddAll()
	if _, err := r.Commit("one"); err != nil {
		t.Fatal(err)
	}
	first, _ := r.Run("rev-parse", "HEAD")
	first = strings.TrimSpace(first)
	write(t, r, "a", []byte("2\n"))
	_ = r.AddAll()
	if _, err := r.Commit("two"); err != nil {
		t.Fatal(err)
	}
	second, _ := r.Run("rev-parse", "HEAD")
	second = strings.TrimSpace(second)
	if ok, err := r.IsAncestor(first, second); err != nil || !ok {
		t.Errorf("IsAncestor(first, second) = %v, %v", ok, err)
	}
	if ok, err := r.IsAncestor(second, first); err != nil || ok {
		t.Errorf("IsAncestor(second, first) = %v, %v", ok, err)
	}
	if err := r.ConfigSet("user.nickname", "x y"); err != nil {
		t.Fatal(err)
	}
	if out, err := r.RunClean("config", "--get", "user.nickname"); err != nil || out != "x y\n" {
		t.Errorf("RunClean config = %q, %v", out, err)
	}
	if out, err := r.RunEnv([]string{"GIT_AUTHOR_NAME=zed"}, nil, "var", "GIT_AUTHOR_IDENT"); err != nil || !contains(out, "zed") {
		t.Errorf("RunEnv did not pass the variable: %q, %v", out, err)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func TestLocalOnlyPath(t *testing.T) {
	yes := []string{"config.json", "daemon.pid", "denylist.txt", "distilled/DISTILLED.md",
		".state/hook.lock", "x.tmp", "x.tmp.1", "p/.f.md.ab12.tmp", "p/f.md.from-remote-abcdef12"}
	no := []string{"MEMORY.md", "p/ideas.tmp.md", "p/x.tmp", "distilled/other.md", "config.jsonl", "p/.tmp"}
	for _, p := range yes {
		if !LocalOnlyPath(p) {
			t.Errorf("LocalOnlyPath(%q) = false, want true", p)
		}
	}
	for _, p := range no {
		if LocalOnlyPath(p) {
			t.Errorf("LocalOnlyPath(%q) = true, want false", p)
		}
	}
}
