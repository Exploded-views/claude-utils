//go:build budget

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/proc"
)

// B8: a `changes --hook` run starts at most 10 git processes on the B1 fixture
// (five repos). Counted two ways that must agree with the budget: through the
// proc seam every git child goes through, and by a git shim on PATH that sees
// every git process whoever starts it. Not load-gated: it is a count.
//
// Two runs: the usual prompt, when nothing is new since the markers (the case
// to make nearly free), and the first prompt after news in every repo (logged;
// every repo then needs its diff).
func TestBudgetB8_HookGitSpawns(t *testing.T) {
	logLoad(t)
	f := newHookProcFixture(t)
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	shim := "#!/bin/sh\necho x \"$@\" >> " + calls + "\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(shim), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	count := func(name string) (seam, shimmed int) {
		var n atomic.Int64
		restore := proc.SetSpawnHook(func(string, []string) { n.Add(1) })
		defer restore()
		os.Remove(calls)
		code, out, errs := runHook(t, f.stdin, "--hook", "--deadline", "0")
		if code != 0 || errs != "" {
			t.Fatalf("%s: exit %d stderr %q", name, code, errs)
		}
		b, _ := os.ReadFile(calls)
		shimmed = strings.Count(string(b), "x ")
		if testing.Verbose() {
			seen := map[string]int{}
			for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
				f := strings.Fields(l)
				if len(f) > 3 {
					seen[strings.Join(f[3:min(len(f), 7)], " ")]++
				}
			}
			for k, v := range seen {
				t.Logf("    %3d  %s", v, k)
			}
		}
		t.Logf("B8 %s: %d git spawns through the proc seam, %d seen by the PATH shim (report %d bytes)", name, int(n.Load()), shimmed, len(out))
		return int(n.Load()), shimmed
	}

	// News in every repo: report it (markers move to HEAD).
	news, newsShim := count("first prompt after news")
	// Nothing new now.
	seam, shimmed := count("nothing new since the markers")
	if seam > 10 || shimmed > 10 {
		t.Errorf("B8: %d spawns through the seam, %d by the shim, budget 10", seam, shimmed)
	}
	// The seam must see every git the shim sees, or the count means nothing.
	if news != newsShim || seam != shimmed {
		t.Errorf("B8: the proc seam counted %d and %d spawns, the PATH shim %d and %d", news, seam, newsShim, shimmed)
	}
	// With news in all five repos each needs its batch-check, merge-base, diff and
	// config read, and the route lookup adds four: keep it from creeping back.
	if news > 30 {
		t.Errorf("B8: the first prompt after news started %d git processes, want at most 30 (was 75)", news)
	}
}
