package sync

import (
	"os"
	"path/filepath"
	"runtime"
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
// conflict; the work-tree must not be left mid-rebase, the conflicted file must
// never reach the Claude tree with markers, and unrelated notes must still
// sync both ways. This PC's version wins; the other PC's copy is kept beside
// the mirror file as .from-remote-*.
func TestRebaseConflict_CommittedFileLocalWinsAndSyncContinues(t *testing.T) {
	f := newPullFixture(t, map[string]string{"clean.md": "a\nb\n"})
	logs := captureLog(t)
	f.editLocal("clean.md", "a\nlocal\n")
	f.peerPush("clean.md", "a\npeer\n")
	f.peerPush("peer1.md", "peer one\n")

	err := f.loop.flush(f.repo, f.roots, true)
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got, _ := f.claudeCopy("clean.md"); got != "a\nlocal\n" {
		t.Errorf("Claude copy of the conflicted note = %q, want the local version", got)
	}
	if inProgress(f) {
		t.Error("work-tree left mid-rebase or mid-merge")
	}
	if b := f.backups("clean.md"); len(b) != 1 {
		t.Errorf("want the peer's version kept once as .from-remote-*, got %v", b)
	} else if c, _ := os.ReadFile(b[0]); string(c) != "a\npeer\n" {
		t.Errorf("backup = %q, want the peer's version", c)
	}
	if got, ok := f.claudeCopy("peer1.md"); !ok || got != "peer one\n" {
		t.Errorf("unrelated peer note not pulled")
	}
	if !f.originHas("projects/p1/memory/clean.md") {
		t.Error("nothing pushed")
	}
	if !strings.Contains(logs.String(), "1 file(s)") || strings.Contains(logs.String(), "clean.md") {
		t.Errorf("want the conflict logged by count, never by path; log:\n%s", logs)
	}

	// Later cycles keep flowing.
	f.editLocal("later.md", "later\n")
	f.peerPush("peer2.md", "peer two\n")
	if err := f.loop.flush(f.repo, f.roots, true); err != nil {
		t.Fatalf("second flush: %v", err)
	}
	if !f.originHas("projects/p1/memory/later.md") {
		t.Error("later.md never pushed")
	}
	if _, ok := f.claudeCopy("peer2.md"); !ok {
		t.Error("peer2.md never pulled")
	}
}

// The peer deletes a note this PC edited: a modify/delete conflict, which a
// content strategy cannot settle. This PC's version is kept.
func TestRebaseConflict_ModifyDelete(t *testing.T) {
	f := newPullFixture(t, map[string]string{"clean.md": "a\nb\n", "other.md": "o\n"})
	captureLog(t)
	f.editLocal("clean.md", "a\nlocal\n")
	f.peerPush("clean.md", "")
	f.peerPush("peer1.md", "peer one\n")

	if err := f.loop.flush(f.repo, f.roots, true); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got, ok := f.claudeCopy("clean.md"); !ok || got != "a\nlocal\n" {
		t.Errorf("Claude copy = %q, ok=%v, want the local version", got, ok)
	}
	if inProgress(f) {
		t.Error("work-tree left mid-rebase or mid-merge")
	}
	if _, ok := f.claudeCopy("peer1.md"); !ok {
		t.Error("peer1.md not pulled")
	}
}

// When even the fallback merge fails, the rebase is aborted, the local commit
// is kept, nothing is propagated into the Claude tree, and every cycle logs
// the conflict by count until it resolves.
func TestRebaseConflict_FallbackFailsAbortsAndLogsEveryCycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell hook")
	}
	f := newPullFixture(t, map[string]string{"clean.md": "a\nb\n"})
	logs := captureLog(t)
	hook := filepath.Join(f.repo.Dir, ".git", "hooks", "pre-merge-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	f.editLocal("clean.md", "a\nlocal\n")
	f.peerPush("clean.md", "a\npeer\n")
	f.peerPush("peer1.md", "peer one\n")

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
	}

	// A later edit resolves it once the fallback can work again.
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	if err := f.loop.flush(f.repo, f.roots, true); err != nil {
		t.Fatalf("after the hook is gone: %v", err)
	}
	if _, ok := f.claudeCopy("peer1.md"); !ok {
		t.Error("peer1.md never pulled after the conflict cleared")
	}
}
