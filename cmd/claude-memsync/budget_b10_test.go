//go:build budget

package main

import (
	"strings"
	"testing"
	"time"
)

// B10: the hook converges at 40 repos with the configs a real host has
// (mergeDriverPath and a token command set, as init writes them). A count, not
// a timing: a per-repo constant of git processes times 40 is the livelock
// where every hook hit its deadline before any repo was looked at.
func TestBudgetB10_HooksConvergeAt40(t *testing.T) {
	logLoad(t)
	const n = 40
	cfgs, names := realisticHost(t, n)
	for _, name := range names {
		noteFor(t, cfgs, name, "base")
	}
	requireTrusted(t, cfgs)
	s := gitShim(t, "")
	in := hookJSON("b10", "", "UserPromptSubmit")

	seam, shimmed, out, _ := countedHook(t, s, in, "--deadline", "0")
	t.Logf("B10 new session: %d spawns through the seam, %d by the shim", seam, shimmed)
	requireSpawns(t, "a new session's first look", seam, shimmed, 0)
	if out != "" {
		t.Errorf("a baseline printed %q", out)
	}
	seam, shimmed, _, _ = countedHook(t, s, in, "--deadline", "0")
	t.Logf("B10 steady state: %d spawns through the seam, %d by the shim", seam, shimmed)
	requireSpawns(t, "steady state, nothing new", seam, shimmed, 0)

	// News in every repo, untimed: five processes per repo and the route lookup.
	notes := map[string]string{}
	for _, name := range names {
		notes[name] = noteFor(t, cfgs, name, "news1")
	}
	seam, shimmed, out, _ = countedHook(t, s, in, "--deadline", "0")
	t.Logf("B10 news in all %d repos: %d spawns through the seam, %d by the shim", n, seam, shimmed)
	if limit := 5*n + 4; seam > limit || shimmed > limit {
		t.Errorf("B10: news in %d repos started %d / %d git processes, budget %d", n, seam, shimmed, limit)
	}
	if seam != shimmed {
		t.Errorf("B10: the seam counted %d, the shim %d", seam, shimmed)
	}
	for _, name := range names {
		if got := strings.Count(hookContext(t, out), notes[name]); got != 1 {
			t.Errorf("repo %s: %q reported %d times, want 1", name, notes[name], got)
		}
	}

	// News again, under the real default deadline and a git that costs about
	// what it does on a Mac (1.52 s over 197 processes): every notice is
	// printed once, within 6 calls, and then the hook is free.
	for _, name := range names {
		notes[name] = noteFor(t, cfgs, name, "news2")
	}
	s = gitShim(t, "0.008")
	wait := workerSync(t)
	printed := map[string]int{}
	calls := 0
	for ; calls < 6; calls++ {
		start := time.Now()
		_, out, _ := runHook(t, in, "--hook")
		took := time.Since(start)
		wait()
		t.Logf("B10 convergence call %d: %v, %d bytes", calls+1, took, len(out))
		settled := true
		for _, name := range names {
			printed[name] += strings.Count(hookContext(t, out), notes[name])
			if markerOf(cfgs[name], "b10") != fileHead(cfgs[name]) {
				settled = false
			}
		}
		if settled {
			calls++
			break
		}
	}
	for _, name := range names {
		if printed[name] != 1 {
			t.Errorf("repo %s: %q printed %d times over %d calls, want 1", name, notes[name], printed[name], calls)
		}
	}
	start := time.Now()
	seam, shimmed, out, _ = countedHook(t, s, in)
	took := time.Since(start)
	t.Logf("B10 after convergence: %d spawns, %v", seam, took)
	requireSpawns(t, "the call after convergence", seam, shimmed, 0)
	if out != "" {
		t.Errorf("the call after convergence printed %q", out)
	}
	// Wall time is gated only on a quiet machine (the timing gates' rule).
	if load := loadAvgFn(); took >= 200*time.Millisecond {
		if load > maxLoad {
			t.Logf("the call after convergence took %v at load %.2f: not judged", took, load)
		} else {
			t.Errorf("the call after convergence took %v at load %.2f, want under 200ms", took, load)
		}
	}
}
