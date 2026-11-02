package sync

import "testing"

// A fetch that is not followed by a merge (a manual `git fetch`, another
// tool's fetch, a daemon pull whose rebase aborted) leaves HEAD behind
// refs/remotes/origin/<branch>. The idle gate must not call that idle, or the
// remote commits are never merged and never reach Claude's memory folders.
func TestIdle_FetchedButNotMergedIsNotIdle(t *testing.T) {
	f := newPullFixture(t, map[string]string{"clean.md": "a\n"})
	captureLog(t)
	f.peerPush("new.md", "from peer\n")
	mustGit(t, f.repo, "fetch", "-q", "origin")

	idle, err := remoteAndLocalIdle(f.repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if idle {
		t.Error("HEAD is behind origin/main after a bare fetch, but the repo was reported idle")
	}

	f.loop.pullTick(f.repo, f.roots)

	if got := f.originHas("projects/p1/memory/new.md"); !got {
		t.Fatal("setup: new.md not on origin/main")
	}
	if _, err := f.repo.Run("merge-base", "--is-ancestor", "origin/main", "HEAD"); err != nil {
		t.Errorf("HEAD does not contain origin/main after the pull tick: %v", err)
	}
	if got, ok := f.claudeCopy("new.md"); !ok || got != "from peer\n" {
		t.Errorf("note did not reach the Claude memory folder: %q, %v", got, ok)
	}
}

func TestIdle_RemoteAndLocalIdle(t *testing.T) {
	commitLocal := func(f *pullFixture, name string) {
		f.t.Helper()
		f.editLocal(name, name+"\n")
		mustGit(f.t, f.repo, "add", "-A")
		mustGit(f.t, f.repo, "commit", "-q", "-m", "local "+name)
	}
	for _, tc := range []struct {
		name  string
		setup func(f *pullFixture)
		want  bool
	}{
		{"level", func(f *pullFixture) {}, true},
		{"ahead", func(f *pullFixture) { commitLocal(f, "l.md") }, false},
		{"behind", func(f *pullFixture) {
			f.peerPush("p.md", "p\n")
			mustGit(f.t, f.repo, "fetch", "-q", "origin")
		}, false},
		{"diverged", func(f *pullFixture) {
			commitLocal(f, "l.md")
			f.peerPush("p.md", "p\n")
			mustGit(f.t, f.repo, "fetch", "-q", "origin")
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPullFixture(t, map[string]string{"clean.md": "a\n"})
			captureLog(t)
			tc.setup(f)
			got, err := remoteAndLocalIdle(f.repo, "main")
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("remoteAndLocalIdle = %v, want %v", got, tc.want)
			}
		})
	}
}
