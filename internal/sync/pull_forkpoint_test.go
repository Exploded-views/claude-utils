package sync

import (
	"os"
	"path/filepath"
	"testing"
)

// noRefLogs turns ref logging off in the sync repo and deletes every reflog,
// as a repo cloned under a global core.logAllRefUpdates=false would be.
func (f *pullFixture) noRefLogs() {
	f.t.Helper()
	mustGit(f.t, f.repo, "config", "core.logAllRefUpdates", "false")
	if err := os.RemoveAll(filepath.Join(f.repo.Dir, ".git", "logs")); err != nil {
		f.t.Fatal(err)
	}
}

// --fork-point must not drop a legitimate local commit: this PC commits, the
// remote moves forward (no force-push), the pull replays the local commit on
// top and the push publishes it.
func TestPull_ForkPointKeepsLocalCommitWhenRemoteMovesOn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		noLogs bool
	}{{"reflogs", false}, {"no reflogs", true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPullFixture(t, map[string]string{"clean.md": "a\n"})
			captureLog(t)
			if tc.noLogs {
				f.noRefLogs()
			}
			// Some history in origin/main's reflog first.
			f.peerPush("p1.md", "1\n")
			f.loop.pullTick(f.repo, f.roots)

			f.editLocal("clean.md", "a\nlocal\n")
			if _, err := CommitMirror(f.repo, f.loop.Cfg, "local"); err != nil {
				t.Fatal(err)
			}
			f.peerPush("p2.md", "2\n")
			if err := f.loop.flush(f.repo, f.roots, false); err != nil {
				t.Fatalf("flush: %v", err)
			}
			if got := f.originFile("clean.md"); got != "a\nlocal\n" {
				t.Errorf("local commit lost: origin clean.md = %q", got)
			}
			if f.originFile("p2.md") != "2\n" || f.originFile("p1.md") != "1\n" {
				t.Errorf("peer commits lost on origin")
			}
		})
	}
}

// The purge case with no reflogs at all (core.logAllRefUpdates=false): the
// daemon's own fetches must write origin/main's reflog, or --fork-point has
// nothing to work from and the purged file is replayed and pushed back.
func TestPull_ForcePushedPurgeIsNotRepublishedWithoutRefLogs(t *testing.T) {
	f := newPullFixture(t, map[string]string{"clean.md": "a\n"})
	captureLog(t)
	f.noRefLogs()
	f.peerPush("leak.md", "oops\n")
	f.loop.pullTick(f.repo, f.roots)
	if !f.originHas("projects/p1/memory/leak.md") {
		t.Fatal("setup: leak not on origin")
	}
	f.editLocal("clean.md", "a\nlocal\n")
	if _, err := CommitMirror(f.repo, f.loop.Cfg, "local"); err != nil {
		t.Fatal(err)
	}
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
	if f.originFile("clean.md") != "a\nlocal\n" {
		t.Errorf("local commit lost")
	}
}

func TestIsRebaseStart(t *testing.T) {
	for s, want := range map[string]bool{
		"memsync-pull 0123abcd (start): checkout origin/main":             true,
		"rebase (start): checkout origin/main":                            true,
		"rebase -i (start): checkout main~3":                              true,
		"pull --rebase https://h/r.git main (start): checkout FETCH_HEAD": true,
		"memsync-pull 0123abcd (pick): notes (start): kickoff":            false,
		"rebase (pick): x (start): y":                                     false,
		"memsync-pull 0123abcd (finish): returning to refs/heads/main":    false,
		"rebase (continue): resolved":                                     false,
		"rebase: checkout origin/main":                                    false, // pre-2.26 apply backend
		"commit: plain":                                                   false,
		"commit (amend): (start)":                                         false,
		"reset: moving to HEAD~1":                                         false,
		"":                                                                false,
	} {
		if got := isRebaseStart(s); got != want {
			t.Errorf("isRebaseStart(%q) = %v, want %v", s, got, want)
		}
	}
}
