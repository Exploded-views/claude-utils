package sync

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Review proof (round 7, uncommitted). A peer purges a leaked file from the
// remote by rewriting history and force-pushing. `git pull --rebase` used the
// fork point of origin/main's reflog and dropped the old upstream commits;
// `git rebase origin/main` replays them and the daemon pushes the purged file
// back.
func TestPull_ForcePushedPurgeIsNotRepublished(t *testing.T) {
	f := newPullFixture(t, map[string]string{"clean.md": "a\n"})
	captureLog(t)
	f.peerPush("leak.md", "oops\n")
	f.loop.pullTick(f.repo, f.roots)
	if !f.originHas("projects/p1/memory/leak.md") {
		t.Fatal("setup: leak not on origin")
	}
	// A local commit not yet pushed when the purge lands.
	f.editLocal("clean.md", "a\nlocal\n")
	if _, err := CommitMirror(f.repo, f.loop.Cfg, "local"); err != nil {
		t.Fatal(err)
	}
	// The peer purges the leak and force-pushes.
	mustGit(t, f.peer, "pull", "-q", "origin", "main")
	mustGit(t, f.peer, "reset", "-q", "--hard", "HEAD~1")
	writeRepoFile(t, f.peer, "projects/p1/memory/other.md", "o\n")
	mustGit(t, f.peer, "add", "-A")
	mustGit(t, f.peer, "commit", "-q", "-m", "replacement")
	mustGit(t, f.peer, "push", "-q", "-f", "origin", "main")

	if err := f.loop.flush(f.repo, f.roots, false); err != nil {
		t.Fatalf("flush: %v", err)
	}
	mustGit(t, f.repo, "fetch", "-q", "origin")
	if f.originHas("projects/p1/memory/leak.md") {
		log, _ := f.repo.Run("log", "--format=%s", "origin/main")
		t.Errorf("the purged file was pushed back to origin:\n%s", log)
	}
}

// with no HEAD reflog (core.logAllRefUpdates=false before the
// repo had one), the daemon cannot recognise its own conflicted rebase, so an
// ordinary pull conflict is left stopped and the daemon pauses, saying the user
// started it.
func TestPull_NoReflogDaemonConflictIsAborted(t *testing.T) {
	f := newPullFixture(t, map[string]string{"clean.md": "a\nb\n"})
	captureLog(t)
	mustGit(t, f.repo, "config", "core.logAllRefUpdates", "false")
	if err := os.Remove(filepath.Join(f.repo.Dir, ".git", "logs", "HEAD")); err != nil {
		t.Fatal(err)
	}
	f.editLocal("clean.md", "a\nlocal\n")
	if _, err := CommitMirror(f.repo, f.loop.Cfg, "c"); err != nil {
		t.Fatal(err)
	}
	f.peerPush("clean.md", "a\npeer\n")
	err := f.loop.flush(f.repo, f.roots, false)
	if errors.Is(err, errHandResolving) || f.repo.RebaseInProgress() {
		t.Errorf("daemon's own conflicted rebase left stopped and taken for the user's: err=%v inProgress=%v", err, f.repo.RebaseInProgress())
	}
}

// a replayed commit whose subject contains " (start): " is
// recorded as "<action> (pick): <subject>", which the scan takes for the newest
// rebase start, so a later conflict in the same daemon rebase is not
// recognised as the daemon's.
func TestPull_PickSubjectWithStartIsNotTakenForStart(t *testing.T) {
	f := newPullFixture(t, map[string]string{"clean.md": "a\nb\n", "other.md": "o\n"})
	captureLog(t)
	writeRepoFile(t, f.repo, "projects/p1/memory/other.md", "o\nmine\n")
	mustGit(t, f.repo, "add", "-A")
	mustGit(t, f.repo, "commit", "-q", "-m", "notes (start): kickoff")
	writeRepoFile(t, f.repo, "projects/p1/memory/clean.md", "a\nlocal\n")
	mustGit(t, f.repo, "add", "-A")
	mustGit(t, f.repo, "commit", "-q", "-m", "second")
	f.peerPush("clean.md", "a\npeer\n")
	err := f.loop.flush(f.repo, f.roots, false)
	if errors.Is(err, errHandResolving) || f.repo.RebaseInProgress() {
		subj, _ := f.repo.ReflogSubjects(5)
		t.Errorf("daemon's own rebase not recognised: err=%v reflog=%q", err, strings.Join(subj, " | "))
	}
}
