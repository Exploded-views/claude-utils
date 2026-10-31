//go:build !windows

package gitwt

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitCalls runs fn with a git shim on PATH and returns the git argv lines it
// saw. The daemon's start-up spawn count is budgeted (budget test B9), so the
// guards added for per-PC files are pinned here to the spawns they need.
func gitCalls(t *testing.T, fn func()) []string {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	shim := "#!/bin/sh\necho \"$@\" >> " + log + "\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(shim), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	fn()
	b, _ := os.ReadFile(log)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestEnsureExclude_NeedsNoSpawnInAPlainRepo(t *testing.T) {
	r := testRepo(t)
	calls := gitCalls(t, func() {
		if err := r.EnsureExclude(LocalOnlyExcludes...); err != nil {
			t.Fatal(err)
		}
	})
	if len(calls) != 1 || calls[0] != "" {
		t.Errorf("EnsureExclude spawned git: %q", calls)
	}
	if b, _ := os.ReadFile(filepath.Join(r.Dir, ".git", "info", "exclude")); !strings.Contains(string(b), "/config.json") {
		t.Errorf("exclude not written: %q", b)
	}
}

// The guarded commit lists the index once, in place of Commit's own probe: a
// clean commit is two git calls (list, commit), and an empty one is one.
func TestCommitGuarded_ListsTheIndexOnce(t *testing.T) {
	r := testRepo(t)
	os.WriteFile(filepath.Join(r.Dir, "a.md"), []byte("a"), 0o600)
	if _, err := r.Run("add", "a.md"); err != nil {
		t.Fatal(err)
	}
	var made bool
	calls := gitCalls(t, func() {
		var err error
		if made, err = r.CommitGuarded("x"); err != nil {
			t.Fatal(err)
		}
	})
	if !made || len(calls) != 2 {
		t.Errorf("made=%v calls=%q, want a commit in 2 git calls", made, calls)
	}
	calls = gitCalls(t, func() {
		var err error
		if made, err = r.CommitGuarded("again"); made || err != nil {
			t.Errorf("an empty index is no commit and no error: made=%v err=%v", made, err)
		}
	})
	if len(calls) != 1 {
		t.Errorf("an empty guarded commit took %d git calls: %q", len(calls), calls)
	}
}

// In a worktree .git is a file; git says where the exclude file is.
func TestEnsureExclude_AsksGitForAGitfile(t *testing.T) {
	r := testRepo(t)
	if _, err := r.Run("commit", "--allow-empty", "-q", "-m", "root"); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	if _, err := r.Run("worktree", "add", "-q", "-b", "other", wt); err != nil {
		t.Skip(err)
	}
	w := New(wt)
	if err := w.EnsureExclude(LocalOnlyExcludes...); err != nil {
		t.Fatalf("a worktree's exclude file: %v", err)
	}
	out, _ := w.Run("check-ignore", "config.json")
	if strings.TrimSpace(out) != "config.json" {
		t.Errorf("config.json is not ignored in the worktree: %q", out)
	}
}
