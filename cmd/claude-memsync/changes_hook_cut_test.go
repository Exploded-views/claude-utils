package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/proc"
)

// A repo whose git was killed in the middle of its last step (the local-copy
// comparison swallows the error and returns a report without it) is not a
// finished repo: it is not announced and its marker does not move. The cut
// closes the record of finished repos before it kills anything.
func TestHook_RepoCutMidStepIsNotPublished(t *testing.T) {
	cfgs, names := realisticHost(t, 3)
	in := hookJSON("s-mid", "", "UserPromptSubmit")
	runHook(t, in, "--hook", "--deadline", "0")
	notes := map[string]string{}
	for _, name := range names {
		notes[name] = noteFor(t, cfgs, name, "news")
	}
	// r00 has a local copy that differs, so its report needs the batch compare.
	local := filepath.Join(cfgs["r00"].ClaudeProjectsDir, "f-r00", "memory", notes["r00"])
	if err := os.MkdirAll(filepath.Dir(local), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("my own edit"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := markerOf(cfgs["r00"], "s-mid")
	gitShimScript(t, "case \"$last\" in --batch) sleep 20;; esac\n")
	wait := workerSync(t)
	// Give the killed repo time to finish its goroutine before the record is
	// read: if the record is still open then, the killed repo is published.
	hookKilledTestHook = func() { time.Sleep(600 * time.Millisecond) }
	t.Cleanup(func() { hookKilledTestHook = nil })
	_, out, errs := runHook(t, in, "--hook", "--deadline", "800ms")
	wait()
	ctx := hookContext(t, out)
	if !strings.Contains(errs, "hook deadline exceeded") {
		t.Fatalf("stderr %q: want the deadline path", errs)
	}
	if strings.Contains(ctx, notes["r00"]) {
		t.Errorf("r00 was cut in its last git step but its notice was printed:\n%s", ctx)
	}
	if got := markerOf(cfgs["r00"], "s-mid"); got != base {
		t.Errorf("r00 was cut mid-step but its marker moved from %q to %q", base, got)
	}
	for _, name := range []string{"r01", "r02"} {
		if !strings.Contains(ctx, notes[name]) {
			t.Errorf("%s finished before the cut but its notice is missing:\n%s", name, ctx)
		}
	}
}

// The deadline guarantee: no git outlives the hook. After a cut nothing is in
// flight, and the stale worker, once its blocked repos are let go, starts no
// new git.
func TestHook_NoGitStartsAfterTheCut(t *testing.T) {
	cfgs, names := realisticHost(t, 12)
	in := hookJSON("s-cut", "", "UserPromptSubmit")
	runHook(t, in, "--hook", "--deadline", "0")
	for _, name := range names {
		noteFor(t, cfgs, name, "news")
	}
	gitShim(t, "1")
	var starts, ends atomic.Int64
	restore := proc.SetSpawnHook(func(string, []string) { starts.Add(1) })
	defer restore()
	restoreEnd := proc.SetSpawnEndHook(func(string, []string) { ends.Add(1) })
	defer restoreEnd()
	wait := workerSync(t)
	release := make(chan struct{})
	hookRepoTestHook = func(name string) {
		if name > "r03" {
			<-release // held back: they have not started their git yet
		}
	}
	t.Cleanup(func() { hookRepoTestHook = nil })
	_, _, errs := runHook(t, in, "--hook", "--deadline", "400ms")
	if !strings.Contains(errs, "hook deadline exceeded") {
		t.Fatalf("stderr %q: want the deadline path", errs)
	}
	started := starts.Load()
	if started == 0 {
		t.Fatal("no git was in flight at the cut: the test proves nothing")
	}
	for i := 0; i < 100 && ends.Load() < started; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if ends.Load() < started {
		t.Errorf("%d git processes started, %d ended: some outlive the hook", started, ends.Load())
	}
	// While the stale worker still runs, git stays refused ...
	if err := proc.Run(exec.Command("true")); !errors.Is(err, proc.ErrStopped) {
		t.Errorf("git is allowed again while the worker is still running: %v", err)
	}
	// ... and even if something re-allowed it, the worker starts no new repo.
	proc.Release()
	close(release) // the held repos would start now
	time.Sleep(500 * time.Millisecond)
	if got := starts.Load(); got != started {
		t.Errorf("%d git processes started after the hook returned (%d before)", got-started, started)
	}
	wait()
	// Once the worker has ended, git is released for the next hook.
	released := false
	for i := 0; i < 100 && !released; i++ {
		released = proc.Run(exec.Command("true")) == nil
		time.Sleep(20 * time.Millisecond)
	}
	if !released {
		t.Error("git stayed refused after the worker ended")
	}
}

// A cut that really happens (git is slow, the deadline is short) loses nothing
// and repeats nothing: over several calls every repo's notice is printed once,
// each call settles some repos, and then the hook is free.
func TestHook_SlowGitCutsConvergeWithoutLossOrRepeat(t *testing.T) {
	const n = 24
	cfgs, names := realisticHost(t, n)
	in := hookJSON("s-slow", "", "UserPromptSubmit")
	runHook(t, in, "--hook", "--deadline", "0")
	notes := map[string]string{}
	for _, name := range names {
		notes[name] = noteFor(t, cfgs, name, "news")
	}
	s := gitShim(t, "0.05") // a repo needs about 5 git calls: 0.25 s, 24 repos, 8 at a time
	wait := workerSync(t)
	printed := map[string]int{}
	cuts, calls := 0, 0
	for ; calls < 20 && pendingRepos(cfgs, names, "s-slow") > 0; calls++ {
		before := pendingRepos(cfgs, names, "s-slow")
		_, out, errs := runHook(t, in, "--hook", "--deadline", "700ms")
		wait()
		if strings.Contains(errs, "hook deadline exceeded") {
			cuts++
		}
		ctx := hookContext(t, out)
		for _, name := range names {
			printed[name] += strings.Count(ctx, notes[name])
		}
		if after := pendingRepos(cfgs, names, "s-slow"); after >= before {
			t.Fatalf("call %d settled nothing (%d repos pending); stderr %q", calls+1, after, errs)
		}
	}
	if cuts == 0 {
		t.Error("no call was cut: the test did not exercise the deadline path")
	}
	for _, name := range names {
		if printed[name] != 1 {
			t.Errorf("repo %s: notice printed %d times over %d calls (%d cut), want 1", name, printed[name], calls, cuts)
		}
	}
	seam, shimmed, out, _ := countedHook(t, s, in, "--deadline", "0")
	requireSpawns(t, "the call after convergence", seam, shimmed, 0)
	if out != "" {
		t.Errorf("the call after convergence printed %q", out)
	}
}
