package sync

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
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

// markDaemonPull is what the daemon writes before the rebase of its own pull:
// the marker, holding the nonce that names the rebase in the reflog. It returns
// the nonce. Call it before the rebase starts.
func markDaemonPull(t *testing.T, f *pullFixture) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(f.repo.Dir, ".state"), 0700); err != nil {
		t.Fatal(err)
	}
	nonce, err := markPull(f.repo)
	if err != nil {
		t.Fatal(err)
	}
	return nonce
}

// crashedMerge leaves a conflicted merge behind. The daemon's pull is a rebase,
// so a merge is always one the user started.
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
	return f
}

// crashedRebase leaves the daemon's own stopped rebase behind with nothing
// parked: the kill came during a pull on a clean tree.
func crashedRebase(t *testing.T) *pullFixture {
	t.Helper()
	f := newPullFixture(t, map[string]string{"clean.md": "a\nb\n"})
	captureLog(t)
	f.editLocal("clean.md", "a\nlocal\n")
	if _, err := CommitMirror(f.repo, f.loop.Cfg, "c"); err != nil {
		t.Fatal(err)
	}
	f.peerPush("clean.md", "a\npeer\n")
	if err := runPull(f.repo, "main"); err == nil {
		t.Fatal("expected the pull to stop on a conflict")
	}
	if !f.repo.RebaseInProgress() {
		t.Fatal("expected a stopped rebase")
	}
	if parkedOnDisk(f) {
		t.Fatal("nothing should be parked")
	}
	return f
}

// userRebase is a rebase the user started by hand in the sync repo, as the docs
// say to, and is resolving: the same stopped state as crashedRebase, but git's
// own reflog name and no marker, because the daemon did not start it.
func userRebase(t *testing.T) *pullFixture {
	t.Helper()
	f := newPullFixture(t, map[string]string{"clean.md": "a\nb\n"})
	captureLog(t)
	f.editLocal("clean.md", "a\nlocal\n")
	if _, err := CommitMirror(f.repo, f.loop.Cfg, "c"); err != nil {
		t.Fatal(err)
	}
	f.peerPush("clean.md", "a\npeer\n")
	t.Setenv("GIT_EDITOR", "true")
	if _, err := f.repo.Run("pull", "--rebase", "--autostash", "origin", "main"); err == nil || !f.repo.RebaseInProgress() {
		t.Fatal("setup: want a stopped rebase")
	}
	return f
}

// A merge is never the daemon's (its pull is a rebase): it is left alone, and its
// conflict markers are never staged, committed or pushed.
func TestInterrupted_MergeIsLeftAloneAndNeverShipsMarkers(t *testing.T) {
	f := crashedMerge(t)
	err := f.loop.flush(f.repo, f.roots, true)
	if !errors.Is(err, errHandResolving) {
		t.Errorf("flush: %v", err)
	}
	if !f.repo.MergeInProgress() {
		t.Error("the user's merge was aborted")
	}
	if log, _ := f.repo.Run("log", "-p", "--format=%h", "HEAD"); strings.Contains(log, "+<<<<<<<") {
		t.Errorf("conflict markers were committed:\n%s", log)
	}
	if out, _ := f.repo.Run("show", "origin/main:projects/p1/memory/clean.md"); strings.Contains(out, "<<<<<<<") {
		t.Errorf("conflict markers were pushed to origin:\n%s", out)
	}
}

// The same for a stopped rebase when parked.json does not exist.
func TestInterrupted_DaemonRebaseWithNothingParkedNeverShipsMarkers(t *testing.T) {
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

// A rebase the user starts by hand after the daemon's cycle began (the check at
// the start of a cycle passed) is not recorded as synced, and not aborted. Once
// the user finishes, catch-up propagates the commits that were below the
// mid-rebase HEAD, so the next Reconcile does not push the stale Claude copy
// over the peer's edit.
func TestInterrupted_HandRebaseDuringCycleKeepsPeerEdit(t *testing.T) {
	f := newPullFixture(t, map[string]string{"clean.md": "a\nb\n", "other.md": "o\n"})
	captureLog(t)
	f.loop.pullTick(f.repo, f.roots)
	f.peerPush("clean.md", "a\npeer\n")
	f.peerPush("other.md", "o\npeer edit\n")
	f.editLocal("clean.md", "a\nlocal\n")
	f.loop.pullTick(f.repo, f.roots) // conflict, aborted, local commit kept
	if inProgress(f) {
		t.Fatal("daemon left its rebase")
	}

	// Cycle starts (check passes), then the user runs the documented pull.
	f.loop.startCycle(f.repo, f.roots)
	t.Setenv("GIT_EDITOR", "true")
	if _, err := f.repo.Run("pull", "--rebase", "--autostash", "origin", "main"); err == nil {
		t.Fatal("expected the user's pull to stop")
	}
	before := f.syncedHead()
	if err := f.loop.flush(f.repo, f.roots, true); !errors.Is(err, errHandResolving) {
		t.Errorf("racing flush: %v", err)
	}
	if !f.repo.RebaseInProgress() {
		t.Fatal("the user's rebase was aborted by the daemon")
	}
	if rec := f.syncedHead(); rec != before {
		t.Errorf("record moved to %s while the rebase was stopped", rec)
	}
	// User finishes the resolution, daemon carries on.
	writeRepoFile(t, f.repo, "projects/p1/memory/clean.md", "a\nboth\n")
	mustGit(t, f.repo, "add", "projects/p1/memory/clean.md")
	mustGit(t, f.repo, "rebase", "--continue")
	f.loop.pullTick(f.repo, f.roots)
	f.loop.pullTick(f.repo, f.roots)
	if got := f.originFile("other.md"); got != "o\npeer edit\n" {
		t.Errorf("origin other.md = %q: the peer's edit was reverted", got)
	}
	if got, _ := f.claudeCopy("other.md"); got != "o\npeer edit\n" {
		t.Errorf("Claude other.md = %q", got)
	}
}

// The record is not written while a rebase or merge is stopped or HEAD is
// detached.
func TestRecordHead_WritesNothingMidOperation(t *testing.T) {
	cases := map[string]func(t *testing.T) *pullFixture{
		"rebase": func(t *testing.T) *pullFixture { return userRebase(t) },
		"merge":  func(t *testing.T) *pullFixture { return crashedMerge(t) },
		"detached": func(t *testing.T) *pullFixture {
			f := newPullFixture(t, map[string]string{"clean.md": "a\nb\n"})
			captureLog(t)
			mustGit(t, f.repo, "checkout", "-q", "--detach")
			return f
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			if err := os.Remove(syncedHeadPath(f.repo)); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if err := recordHead(f.repo); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(syncedHeadPath(f.repo)); err == nil {
				t.Errorf("record written: %q", f.syncedHead())
			}
		})
	}
}

// A rebase the user starts after the cycle's first check, before the pull, ends
// the cycle: nothing is parked or reset, nothing is pulled or pushed, no record.
func TestFlush_RebaseStartedBeforePullEndsCycle(t *testing.T) {
	f := newPullFixture(t, map[string]string{"clean.md": "a\nb\n"})
	logs := captureLog(t)
	f.loop.pullTick(f.repo, f.roots)
	f.editLocal("clean.md", "a\nlocal\n")
	if _, err := CommitMirror(f.repo, f.loop.Cfg, "c"); err != nil {
		t.Fatal(err)
	}
	f.peerPush("clean.md", "a\npeer\n")
	f.writeWork("projects/p1/memory/new.md", "uncommitted\n") // a pull would park it
	tip := strings.TrimSpace(mustGit(t, f.repo, "ls-remote", "origin", "refs/heads/main"))
	beforePullCheck = func(r *gitwt.Repo) {
		t.Setenv("GIT_EDITOR", "true")
		if _, err := r.Run("pull", "--rebase", "origin", "main"); err == nil {
			t.Error("expected the user's pull to stop")
		}
	}
	t.Cleanup(func() { beforePullCheck = func(*gitwt.Repo) {} })
	if err := recordHead(f.repo); err != nil { // the commit above is reflected in ~/.claude
		t.Fatal(err)
	}
	rec := f.syncedHead()
	logs.Reset()
	if err := f.loop.flush(f.repo, f.roots, false); !errors.Is(err, errHandResolving) {
		t.Fatalf("flush: %v", err)
	}
	if !f.repo.RebaseInProgress() {
		t.Fatal("the user's rebase was aborted")
	}
	if parkedOnDisk(f) {
		t.Error("parked around a user's rebase")
	}
	if got, _ := f.workFile("projects/p1/memory/new.md"); got != "uncommitted\n" {
		t.Errorf("work-tree file touched: %q", got)
	}
	if pullMarked(f.repo) {
		t.Error("pull marker written")
	}
	if f.syncedHead() != rec {
		t.Error("record moved")
	}
	if now := strings.TrimSpace(mustGit(t, f.repo, "ls-remote", "origin", "refs/heads/main")); now != tip {
		t.Error("origin moved")
	}
	if !strings.Contains(logs.String(), "in progress in the sync repo") {
		t.Errorf("not logged: %s", logs)
	}
}

// git names the rebase after GIT_REFLOG_ACTION, which is how the daemon's rebase
// is told from the user's: the newest start in HEAD's reflog is the marker's.
func TestPullReflog_NamesTheRebaseWithTheNonce(t *testing.T) {
	f := crashedRebase(t)
	nonce := pullNonceOf(t, f)
	subjects, err := f.repo.ReflogSubjects(20)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(subjects[0], "memsync-pull "+nonce+" (start): checkout ") {
		t.Errorf("reflog: %q", subjects)
	}
	if !stoppedByMarkedPull(f.repo) {
		t.Error("the daemon's own rebase was not recognised")
	}
	// The same git, run by the user, names it after the command.
	u := userRebase(t)
	if subjects, _ := u.repo.ReflogSubjects(20); strings.Contains(strings.Join(subjects, "\n"), "memsync-pull") {
		t.Errorf("reflog: %q", subjects)
	}
}

func pullNonceOf(t *testing.T, f *pullFixture) string {
	t.Helper()
	b, err := os.ReadFile(pullMarkerPath(f.repo))
	if err != nil || strings.TrimSpace(string(b)) == "" {
		t.Fatalf("no marker: %v", err)
	}
	return strings.TrimSpace(string(b))
}

// The rebase check that follows a failed pull aborts only a rebase that pull
// started. One that was there before it is left alone, and so is its state.
// This is also the rebase the user starts while the daemon is fetching: the
// marker is written after the fetch, and the daemon's rebase then fails.
func TestAbortRebaseConflict_LeavesPreexistingRebase(t *testing.T) {
	f := userRebase(t)   // the user's rebase, no marker
	markDaemonPull(t, f) // a pull marked afterwards: the rebase predates it
	err := abortRebaseConflict(f.repo)
	if !errors.Is(err, errHandResolving) {
		t.Errorf("want errHandResolving, got %v", err)
	}
	if !f.repo.RebaseInProgress() {
		t.Error("the rebase that predates the pull was aborted")
	}
}

// The same through a whole cycle: the user's pull begins between the daemon's
// fetch and its rebase.
func TestFlush_UserRebaseStartedDuringFetchIsLeftAlone(t *testing.T) {
	f := newPullFixture(t, map[string]string{"clean.md": "a\nb\n"})
	logs := captureLog(t)
	f.loop.pullTick(f.repo, f.roots)
	f.editLocal("clean.md", "a\nlocal\n")
	if _, err := CommitMirror(f.repo, f.loop.Cfg, "c"); err != nil {
		t.Fatal(err)
	}
	f.peerPush("clean.md", "a\npeer\n")
	t.Setenv("GIT_EDITOR", "true")
	afterFetch = func(r *gitwt.Repo) {
		if _, err := r.Run("pull", "--rebase", "origin", "main"); err == nil {
			t.Error("expected the user's pull to stop")
		}
	}
	t.Cleanup(func() { afterFetch = func(*gitwt.Repo) {} })
	logs.Reset()
	if err := f.loop.flush(f.repo, f.roots, false); !errors.Is(err, errHandResolving) {
		t.Fatalf("flush: %v", err)
	}
	if !f.repo.RebaseInProgress() {
		t.Fatal("the user's rebase was aborted")
	}
	if pullMarked(f.repo) {
		t.Error("marker left behind")
	}
	if !strings.Contains(logs.String(), "git -C ~/.claudesync rebase --abort") {
		t.Errorf("the log does not say how to give it up:\n%s", logs)
	}
}

func TestAbortRebaseConflict_AbortsItsOwnRebase(t *testing.T) {
	f := crashedRebase(t)
	err := abortRebaseConflict(f.repo)
	if err == nil || errors.Is(err, errHandResolving) || !strings.Contains(err.Error(), "rebase aborted") {
		t.Errorf("got %v", err)
	}
	if f.repo.RebaseInProgress() {
		t.Error("the pull's own rebase was left")
	}
}

// A marker left behind does not claim a rebase the user starts later, however
// it came to be left, and the cycle removes it.
func TestInterrupted_StaleMarkerDoesNotClaimLaterRebase(t *testing.T) {
	setup := func(t *testing.T) *pullFixture {
		f := newPullFixture(t, map[string]string{"clean.md": "a\nb\n", "other.md": "o\n"})
		captureLog(t)
		f.loop.pullTick(f.repo, f.roots)
		return f
	}
	conflict := func(t *testing.T, f *pullFixture) {
		f.editLocal("clean.md", "a\nlocal\n")
		if _, err := CommitMirror(f.repo, f.loop.Cfg, "c"); err != nil {
			t.Fatal(err)
		}
		f.peerPush("clean.md", "a\npeer\n")
	}
	userPull := func(t *testing.T, f *pullFixture) {
		t.Setenv("GIT_EDITOR", "true")
		if _, err := f.repo.Run("pull", "--rebase", "--autostash", "origin", "main"); err == nil || !f.repo.RebaseInProgress() {
			t.Fatal("setup: want a stopped rebase")
		}
	}
	// claude-memsync stop during the fetch: the marker is there, no rebase began.
	// The user then follows the docs from the same HEAD, and the daemon starts.
	t.Run("stopped during the fetch, same HEAD", func(t *testing.T) {
		f := setup(t)
		conflict(t, f)
		markDaemonPull(t, f)
		userPull(t, f)
		writeRepoFile(t, f.repo, "projects/p1/memory/clean.md", "a\nhalf-resolved by hand\n")
		requireUserRebaseSurvives(t, f)
		if got, _ := f.workFile("projects/p1/memory/clean.md"); got != "a\nhalf-resolved by hand\n" {
			t.Errorf("the user's work-tree file was touched: %q", got)
		}
	})
	// The daemon's pull finished its rebase, and the kill came before the marker
	// was removed: the nonce is in the reflog, as a finished rebase's start.
	t.Run("rebase finished, marker not removed", func(t *testing.T) {
		f := setup(t)
		f.peerPush("other.md", "o\npeer\n")
		if err := runPull(f.repo, "main"); err != nil {
			t.Fatal(err)
		}
		if !pullMarked(f.repo) {
			t.Fatal("setup: no marker")
		}
		conflict(t, f)
		mustGit(t, f.repo, "fetch", "-q", "origin")
		userPull(t, f)
		requireUserRebaseSurvives(t, f)
	})
	// The marker was written after the user's rebase began.
	t.Run("rebase predates the marker", func(t *testing.T) {
		f := userRebase(t)
		markDaemonPull(t, f)
		requireUserRebaseSurvives(t, f)
	})
}

func requireUserRebaseSurvives(t *testing.T, f *pullFixture) {
	t.Helper()
	logs := captureLog(t)
	f.loop.pullTick(f.repo, f.roots)
	f.restart()
	if !f.repo.RebaseInProgress() {
		t.Errorf("the user's rebase was aborted:\n%s", logs)
	}
	if pullMarked(f.repo) {
		t.Error("the stale marker was kept")
	}
}

// claude-memsync stop removes a marker that no stopped rebase or merge needs. One
// that names the daemon's own stopped rebase stays, for the next start to abort.
func TestClearStalePullMarker(t *testing.T) {
	f := newPullFixture(t, map[string]string{"clean.md": "a\nb\n"})
	captureLog(t)
	markDaemonPull(t, f)
	if err := ClearStalePullMarker(f.loop.Cfg.SyncDir); err != nil {
		t.Fatal(err)
	}
	if pullMarked(f.repo) {
		t.Error("marker kept with nothing stopped")
	}
	if err := ClearStalePullMarker(f.loop.Cfg.SyncDir); err != nil {
		t.Errorf("second call: %v", err)
	}
	d := crashedRebase(t)
	if err := ClearStalePullMarker(d.loop.Cfg.SyncDir); err != nil {
		t.Fatal(err)
	}
	if !pullMarked(d.repo) {
		t.Error("marker removed while the daemon's rebase is stopped")
	}
}

// While the user resolves a rebase by hand the cycle is paused, and says so: the
// result of the flush and the callback are not a success, and the paused state
// is on disk for `claude-memsync status` until the first cycle that goes on.
func TestPaused_ReportedWhileHandResolving(t *testing.T) {
	f := userRebase(t)
	var results []error
	f.loop.OnFlush = func(_ bool, err error) { results = append(results, err) }
	if _, ok := PausedSince(f.repo.Dir); ok {
		t.Fatal("paused before any cycle")
	}
	f.loop.pullTick(f.repo, f.roots)
	if len(results) != 1 || !errors.Is(results[0], errHandResolving) {
		t.Fatalf("OnFlush got %v", results)
	}
	since, ok := PausedSince(f.repo.Dir)
	if !ok || since.IsZero() {
		t.Fatalf("paused = %v, %v", since, ok)
	}
	f.loop.pullTick(f.repo, f.roots)
	if again, _ := PausedSince(f.repo.Dir); !again.Equal(since) {
		t.Errorf("the pause start moved: %v -> %v", since, again)
	}
	mustGit(t, f.repo, "rebase", "--abort")
	f.loop.pullTick(f.repo, f.roots)
	if last := results[len(results)-1]; errors.Is(last, errHandResolving) {
		t.Errorf("still paused after the rebase was given up: %v", last)
	}
	if _, ok := PausedSince(f.repo.Dir); ok {
		t.Error("paused state left behind")
	}
}
