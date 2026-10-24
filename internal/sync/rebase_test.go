package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func inProgress(f *pullFixture) bool {
	for _, d := range []string{"rebase-merge", "rebase-apply", "MERGE_HEAD"} {
		out, _ := f.repo.Run("rev-parse", "--git-path", d)
		p := strings.TrimSpace(out)
		if !filepath.IsAbs(p) {
			p = filepath.Join(f.repo.Dir, p)
		}
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// Both PCs edit the same line of a committed note. The pull rebase stops on a
// conflict. Nothing decides it for the user: the rebase is aborted (the
// work-tree never holds conflict markers), the local commit is kept, nothing
// reaches the Claude tree or origin, and every cycle fails the same way, with
// the conflict given by count and never by path.
func TestRebaseConflict_AbortsKeepsLocalAndBlocksSync(t *testing.T) {
	f := newPullFixture(t, map[string]string{"clean.md": "a\nb\n"})
	logs := captureLog(t)
	f.editLocal("clean.md", "a\nlocal\n")
	f.peerPush("clean.md", "a\npeer\n")
	f.peerPush("peer1.md", "peer one\n")
	tip := strings.TrimSpace(mustGit(t, f.repo, "ls-remote", "origin", "refs/heads/main"))

	for i := 1; i <= 2; i++ {
		logs.Reset()
		err := f.loop.flush(f.repo, f.roots, true)
		if err == nil || !strings.Contains(err.Error(), "1 file(s)") {
			t.Fatalf("cycle %d: want a conflict error naming the count, got %v", i, err)
		}
		if strings.Contains(err.Error(), "clean.md") || strings.Contains(logs.String(), "clean.md") {
			t.Errorf("cycle %d: conflict named a path", i)
		}
		if inProgress(f) {
			t.Fatalf("cycle %d: work-tree left mid-rebase or mid-merge", i)
		}
		if got, _ := f.claudeCopy("clean.md"); got != "a\nlocal\n" {
			t.Errorf("cycle %d: Claude copy = %q", i, got)
		}
		if _, ok := f.claudeCopy("peer1.md"); ok {
			t.Errorf("cycle %d: propagated despite the unresolved conflict", i)
		}
		if show, _ := f.repo.Run("show", "HEAD:projects/p1/memory/clean.md"); show != "a\nlocal\n" {
			t.Errorf("cycle %d: the local commit was not kept: %q", i, show)
		}
		if m, _ := os.ReadFile(filepath.Join(f.roots.Mirror, "p1", "memory", "clean.md")); strings.Contains(string(m), "<<<<<<<") {
			t.Errorf("cycle %d: conflict markers in the mirror", i)
		}
		if now := strings.TrimSpace(mustGit(t, f.repo, "ls-remote", "origin", "refs/heads/main")); now != tip {
			t.Errorf("cycle %d: origin moved while the conflict is unresolved", i)
		}
	}
	if b := f.backups("clean.md"); len(b) != 0 {
		t.Errorf("no side is picked, so no .from-remote-* copies expected, got %v", b)
	}
}

// The peer deletes a note this PC edited: a modify/delete conflict. Same
// handling.
func TestRebaseConflict_ModifyDeleteAborts(t *testing.T) {
	f := newPullFixture(t, map[string]string{"clean.md": "a\nb\n", "other.md": "o\n"})
	captureLog(t)
	f.editLocal("clean.md", "a\nlocal\n")
	f.peerPush("clean.md", "")
	f.peerPush("peer1.md", "peer one\n")

	err := f.loop.flush(f.repo, f.roots, true)
	if err == nil || !strings.Contains(err.Error(), "1 file(s)") {
		t.Fatalf("want a conflict error naming the count, got %v", err)
	}
	if inProgress(f) {
		t.Error("work-tree left mid-rebase or mid-merge")
	}
	if got, ok := f.claudeCopy("clean.md"); !ok || got != "a\nlocal\n" {
		t.Errorf("Claude copy = %q, ok=%v", got, ok)
	}
	if _, ok := f.claudeCopy("peer1.md"); ok {
		t.Error("propagated despite the unresolved conflict")
	}
}

// The documented resolution works: after the abort, `git pull --rebase` in the
// sync dir, fix the file, `git add`, `git rebase --continue`; the next cycle
// pushes the result and pulls the rest.
func TestRebaseConflict_ManualResolutionThenNextCycleSyncs(t *testing.T) {
	f := newPullFixture(t, map[string]string{"clean.md": "a\nb\n"})
	captureLog(t)
	t.Setenv("GIT_EDITOR", "true")
	f.editLocal("clean.md", "a\nlocal\n")
	f.peerPush("clean.md", "a\npeer\n")
	f.peerPush("peer1.md", "peer one\n")
	if err := f.loop.flush(f.repo, f.roots, true); err == nil {
		t.Fatal("want the conflict error")
	}

	// The user's steps from the docs, with the daemon stopped.
	if _, err := f.repo.Run("pull", "--rebase", "origin", "main"); err == nil {
		t.Fatal("expected the pull to stop on the conflict again")
	}
	writeRepoFile(t, f.repo, "projects/p1/memory/clean.md", "a\nboth\n")
	mustGit(t, f.repo, "add", "projects/p1/memory/clean.md")
	mustGit(t, f.repo, "rebase", "--continue")

	if err := f.loop.flush(f.repo, f.roots, true); err != nil {
		t.Fatalf("flush after the resolution: %v", err)
	}
	if !f.originHas("projects/p1/memory/clean.md") {
		t.Error("resolved note never pushed")
	}
	if show := mustGit(t, f.repo, "show", "origin/main:projects/p1/memory/clean.md"); show != "a\nboth\n" {
		t.Errorf("origin has %q", show)
	}
}
