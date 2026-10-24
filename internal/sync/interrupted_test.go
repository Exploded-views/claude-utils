package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// noMarkersAnywhere fails if the conflicted note holds conflict markers in the
// work-tree, in any commit on HEAD, or on origin.
func noMarkersAnywhere(t *testing.T, f *pullFixture) {
	t.Helper()
	const rel = "projects/p1/memory/clean.md"
	if m, _ := os.ReadFile(filepath.Join(f.repo.Dir, filepath.FromSlash(rel))); strings.Contains(string(m), "<<<<<<<") {
		t.Errorf("conflict markers in the work-tree:\n%s", m)
	}
	if log, _ := f.repo.Run("log", "-p", "--format=%h", "HEAD"); strings.Contains(log, "+<<<<<<<") {
		t.Errorf("conflict markers were committed:\n%s", log)
	}
	if out, _ := f.repo.Run("show", "origin/main:"+rel); strings.Contains(out, "<<<<<<<") {
		t.Errorf("conflict markers were pushed to origin:\n%s", out)
	}
	if c, _ := f.claudeCopy("clean.md"); strings.Contains(c, "<<<<<<<") {
		t.Errorf("conflict markers reached the Claude copy:\n%s", c)
	}
}

// markDaemonPull is what the daemon writes before its own pull: the marker
// that says a stopped rebase or merge is the daemon's to abort.
func markDaemonPull(t *testing.T, f *pullFixture) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(f.repo.Dir, ".state"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.repo.Dir, ".state", "pull-running"), nil, 0600); err != nil {
		t.Fatal(err)
	}
}

// crashedMerge leaves a conflicted merge behind, as a kill in the middle of a
// merge would.
func crashedMerge(t *testing.T) *pullFixture {
	t.Helper()
	f := newPullFixture(t, map[string]string{"clean.md": "a\nb\n"})
	captureLog(t)
	f.editLocal("clean.md", "a\nlocal\n")
	if _, err := CommitMirror(f.repo, f.loop.Cfg, "c"); err != nil {
		t.Fatal(err)
	}
	f.peerPush("clean.md", "a\npeer\n")
	mustGit(t, f.repo, "fetch", "-q", "origin")
	if _, err := f.repo.Run("merge", "--no-edit", "origin/main"); err == nil {
		t.Fatal("expected a conflicted merge")
	}
	markDaemonPull(t, f)
	return f
}

// crashedRebase leaves a stopped rebase behind with nothing parked: the kill
// came during a pull on a clean tree.
func crashedRebase(t *testing.T) *pullFixture {
	t.Helper()
	f := newPullFixture(t, map[string]string{"clean.md": "a\nb\n"})
	captureLog(t)
	f.editLocal("clean.md", "a\nlocal\n")
	if _, err := CommitMirror(f.repo, f.loop.Cfg, "c"); err != nil {
		t.Fatal(err)
	}
	f.peerPush("clean.md", "a\npeer\n")
	if err := f.repo.Pull("main"); err == nil {
		t.Fatal("expected the pull to stop on a conflict")
	}
	if !f.repo.RebaseInProgress() {
		t.Fatal("expected a stopped rebase")
	}
	if parkedOnDisk(f) {
		t.Fatal("nothing should be parked")
	}
	markDaemonPull(t, f)
	return f
}

// userRebase is a rebase the user started by hand in the sync repo and is
// resolving: the same stopped state as crashedRebase, but no marker, because
// the daemon did not start it.
func userRebase(t *testing.T) *pullFixture {
	t.Helper()
	f := crashedRebase(t)
	if err := os.Remove(filepath.Join(f.repo.Dir, ".state", "pull-running")); err != nil {
		t.Fatal(err)
	}
	return f
}

// A crash in the middle of a merge must never ship conflict markers: the next
// flush used to `git add` the marked file and conclude the merge.
func TestInterrupted_CrashedMergeNeverShipsMarkers(t *testing.T) {
	f := crashedMerge(t)
	_ = finishParked(f.repo, f.roots) // what a restart did before; it knew nothing of merges
	err := f.loop.flush(f.repo, f.roots, true)
	t.Logf("flush: %v", err)
	if inProgress(f) {
		t.Error("merge left in progress")
	}
	noMarkersAnywhere(t, f)
}

// The same for a stopped rebase when parked.json does not exist.
func TestInterrupted_CrashedRebaseWithNothingParkedNeverShipsMarkers(t *testing.T) {
	f := crashedRebase(t)
	err := f.loop.flush(f.repo, f.roots, true)
	t.Logf("flush: %v", err)
	if inProgress(f) {
		t.Error("rebase left in progress")
	}
	noMarkersAnywhere(t, f)
	if show, _ := f.repo.Run("show", "HEAD:projects/p1/memory/clean.md"); show != "a\nlocal\n" {
		t.Errorf("the local commit was not kept: %q", show)
	}
}

// The startup helper aborts both kinds, and does nothing on a quiet repo.
func TestInterrupted_AbortInterrupted(t *testing.T) {
	for name, mk := range map[string]func(*testing.T) *pullFixture{"merge": crashedMerge, "rebase": crashedRebase} {
		t.Run(name, func(t *testing.T) {
			f := mk(t)
			if err := abortInterrupted(f.repo); err != nil {
				t.Fatal(err)
			}
			if inProgress(f) {
				t.Error("still in progress")
			}
			if err := abortInterrupted(f.repo); err != nil {
				t.Errorf("second call on a quiet repo: %v", err)
			}
		})
	}
}

// Safety net: even if a caller reaches CommitMirror with the merge or rebase
// still unresolved, a file with conflict marker lines is not staged.
func TestCommitMirror_RefusesConflictMarkersWhileUnresolved(t *testing.T) {
	for name, mk := range map[string]func(*testing.T) *pullFixture{"merge": crashedMerge, "rebase": crashedRebase} {
		t.Run(name, func(t *testing.T) {
			f := mk(t)
			before := strings.TrimSpace(mustGit(t, f.repo, "rev-parse", "HEAD"))
			ok, err := CommitMirror(f.repo, f.loop.Cfg, "c")
			if ok || err == nil {
				t.Fatalf("want a refusal, got committed=%v err=%v", ok, err)
			}
			if strings.Contains(err.Error(), "clean.md") {
				t.Errorf("refusal named a path: %v", err)
			}
			if after := strings.TrimSpace(mustGit(t, f.repo, "rev-parse", "HEAD")); after != before {
				t.Error("a commit was made")
			}
			if staged, _ := f.repo.Run("diff", "--cached", "--name-only", "--diff-filter=AM"); strings.Contains(staged, "clean.md") {
				t.Errorf("the marked file was staged: %q", staged)
			}
		})
	}
}

// Unmerged index entries without MERGE_HEAD or a rebase (a conflicted
// cherry-pick) count too.
func TestCommitMirror_RefusesConflictMarkersWithUnmergedEntries(t *testing.T) {
	f := newPullFixture(t, map[string]string{"clean.md": "a\nb\n"})
	captureLog(t)
	f.peerPush("clean.md", "a\npeer\n")
	f.editLocal("clean.md", "a\nlocal\n")
	if _, err := CommitMirror(f.repo, f.loop.Cfg, "c"); err != nil {
		t.Fatal(err)
	}
	mustGit(t, f.repo, "fetch", "-q", "origin")
	if _, err := f.repo.Run("cherry-pick", "origin/main"); err == nil {
		t.Fatal("expected a conflicted cherry-pick")
	}
	if ok, err := CommitMirror(f.repo, f.loop.Cfg, "c"); ok || err == nil {
		t.Fatalf("want a refusal, got committed=%v err=%v", ok, err)
	}
}

// Ordinary files that happen to hold a `=======` line are fine when nothing is
// unresolved.
func TestCommitMirror_MarkerLookalikesCommitWhenQuiet(t *testing.T) {
	f := newPullFixture(t, map[string]string{"a.md": "a\n"})
	captureLog(t)
	f.editLocal("a.md", "Title\n=======\ntext\n")
	ok, err := CommitMirror(f.repo, f.loop.Cfg, "c")
	if err != nil || !ok {
		t.Fatalf("committed=%v err=%v", ok, err)
	}
}

// A rebase the daemon did not start is the user's, who is resolving it by hand.
// A whole cycle (the pull tick, and a restart) leaves it untouched: nothing is
// staged, reconciled, committed or pushed, and the cycle logs it once.
func TestInterrupted_UserStartedRebaseSurvivesCycles(t *testing.T) {
	f := userRebase(t)
	logs := captureLog(t)
	f.peerPush("peer1.md", "peer one\n")
	f.editLocal("new.md", "written meanwhile\n")
	writeMem(t, f.roots.Claude, "p1", "clean.md", "a\nclaude edit\n")
	before := f.head()
	tip := strings.TrimSpace(mustGit(t, f.repo, "ls-remote", "origin", "refs/heads/main"))
	markers, _ := os.ReadFile(filepath.Join(f.roots.Mirror, "p1", "memory", "clean.md"))
	if !strings.Contains(string(markers), "<<<<<<<") {
		t.Fatal("the stopped rebase should hold conflict markers")
	}

	logs.Reset()
	f.loop.pullTick(f.repo, f.roots)
	if n := strings.Count(logs.String(), "in progress in the sync repo"); n != 1 {
		t.Errorf("want one log line per cycle, got %d:\n%s", n, logs)
	}
	f.loop.pullTick(f.repo, f.roots)
	if n := strings.Count(logs.String(), "in progress in the sync repo"); n != 2 {
		t.Errorf("want one log line per cycle, got %d total:\n%s", n, logs)
	}
	f.restart()

	if !f.repo.RebaseInProgress() {
		t.Fatal("the user's rebase was aborted")
	}
	if got, _ := os.ReadFile(filepath.Join(f.roots.Mirror, "p1", "memory", "clean.md")); string(got) != string(markers) {
		t.Errorf("the work-tree file was touched: %q", got)
	}
	if f.head() != before {
		t.Error("HEAD moved")
	}
	if staged, _ := f.repo.Run("diff", "--cached", "--name-only", "--diff-filter=A"); strings.Contains(staged, "new.md") {
		t.Errorf("a file was staged: %q", staged)
	}
	if _, ok := f.claudeCopy("peer1.md"); ok {
		t.Error("Reconcile ran")
	}
	if got, _ := f.claudeCopy("clean.md"); got != "a\nclaude edit\n" {
		t.Errorf("Claude copy = %q", got)
	}
	if now := strings.TrimSpace(mustGit(t, f.repo, "ls-remote", "origin", "refs/heads/main")); now != tip {
		t.Error("origin moved")
	}
}

// A rebase the daemon's own pull left stopped (a crash) is aborted on start, and
// the marker goes with it.
func TestInterrupted_DaemonStartedRebaseAbortedOnStart(t *testing.T) {
	f := crashedRebase(t)
	if !pullMarked(f.repo) {
		t.Fatal("setup: no marker")
	}
	f.restart()
	if inProgress(f) {
		t.Error("the daemon's own stopped rebase was not aborted")
	}
	if pullMarked(f.repo) {
		t.Error("marker left behind")
	}
	noMarkersAnywhere(t, f)
}

// The marker brackets the daemon's pull (a pre-rebase hook sees it) and is gone
// after a normal cycle and after one that stopped on a conflict.
func TestPullMarker_ClearedAfterCycles(t *testing.T) {
	f := newPullFixture(t, map[string]string{"clean.md": "a\nb\n"})
	captureLog(t)
	hook := "#!/bin/sh\n[ -e .state/pull-running ] && touch .state/hook-saw-marker\nexit 0\n"
	if err := os.WriteFile(filepath.Join(f.repo.Dir, ".git", "hooks", "pre-rebase"), []byte(hook), 0700); err != nil {
		t.Fatal(err)
	}
	f.peerPush("other.md", "o\n")
	f.editLocal("clean.md", "a\nlocal\n")
	if err := f.loop.flush(f.repo, f.roots, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.repo.Dir, ".state", "hook-saw-marker")); err != nil {
		t.Error("the marker was not there while the pull ran")
	}
	if pullMarked(f.repo) {
		t.Error("marker left after a normal pull")
	}
	f.peerPush("clean.md", "a\npeer\n")
	f.editLocal("clean.md", "a\nlocal 2\n")
	if err := f.loop.flush(f.repo, f.roots, true); err == nil {
		t.Fatal("want the conflict error")
	}
	if pullMarked(f.repo) {
		t.Error("marker left after an aborted conflict")
	}
}
