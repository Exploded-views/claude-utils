//go:build budget

package main

import (
	"strings"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/config"
)

// B11: the repo count is expected to grow to about 300. Functional only, no
// timing gate (wall time is logged): with real configs, news in every repo and
// the default deadline, every notice is printed exactly once, the calls that
// follow start no git, and the numbers are logged next to B10's. It is behind
// the budget tag because setting up 300 repos takes minutes.
func TestBudgetB11_HooksConvergeAt300(t *testing.T) {
	logLoad(t)
	const n = 300
	cfgs, names := realisticHost(t, n)
	for _, name := range names {
		noteFor(t, cfgs, name, "base")
	}
	sample := map[string]config.Config{}
	for i := 0; i < n; i += 25 {
		sample[names[i]] = cfgs[names[i]]
	}
	requireTrusted(t, sample)
	s := gitShim(t, "")
	in := hookJSON("b11", "", "UserPromptSubmit")

	start := time.Now()
	seam, shimmed, _, _ := countedHook(t, s, in)
	t.Logf("B11 new session at %d repos: %d spawns (seam), %d (shim), %v", n, seam, shimmed, time.Since(start))
	requireSpawns(t, "a new session's first look", seam, shimmed, 0)

	notes := map[string]string{}
	for _, name := range names {
		notes[name] = noteFor(t, cfgs, name, "news")
	}
	wait := workerSync(t)
	printed := map[string]int{}
	const maxCalls = 60
	calls := 0
	for ; calls < maxCalls; calls++ {
		var sm, sh int
		var out string
		start := time.Now()
		sm, sh, out, _ = countedHook(t, s, in) // default deadline
		took := time.Since(start)
		wait()
		t.Logf("B11 convergence call %d: %d spawns (seam), %d (shim), %v, %d bytes", calls+1, sm, sh, took, len(out))
		ctx := hookContext(t, out)
		settled := true
		for _, name := range names {
			printed[name] += strings.Count(ctx, notes[name])
			if markerOf(cfgs[name], "b11") != fileHead(cfgs[name]) {
				settled = false
			}
		}
		if settled {
			calls++
			break
		}
	}
	t.Logf("B11 converged in %d calls", calls)
	for _, name := range names {
		if printed[name] != 1 {
			t.Errorf("repo %s: %q printed %d times over %d calls, want 1", name, notes[name], printed[name], calls)
		}
	}
	for i := 0; i < 3; i++ {
		start := time.Now()
		seam, shimmed, out, _ := countedHook(t, s, in)
		t.Logf("B11 steady call %d: %d spawns (seam), %d (shim), %v", i+1, seam, shimmed, time.Since(start))
		requireSpawns(t, "a steady-state call", seam, shimmed, 0)
		if out != "" {
			t.Errorf("a steady-state call printed %q", out)
		}
	}
}
