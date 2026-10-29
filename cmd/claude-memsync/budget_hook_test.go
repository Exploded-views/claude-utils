//go:build budget

package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/changes"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// B1: `changes --hook` takes under 200 ms (p95 over 20 runs) on the fixture
// tree: a 10k-entry $TMPDIR-shaped dir, 1k project folders, 5 repos.
func TestBudgetB1_HookP95(t *testing.T) {
	cfgs := routedHost(t, budgetRepos, "personal", budgetRoutes())
	tree := newBudgetTree(t, t.TempDir(), 10000)
	t.Setenv("TMPDIR", tree.tmp)

	// Every folder has a note in the projects repo; the hook's project is a real
	// checkout, which has one pending note.
	notes := map[string]string{}
	for _, group := range [][]string{tree.missing, tree.real, tree.other} {
		for _, f := range group {
			notes[f+"/memory/n.md"] = "x"
		}
	}
	addNotes(t, cfgs["projects"].Path(), notes)
	proj := tree.real[0]
	projDir := filepath.Join(tree.tmp, "checkout000")

	type repoBase struct{ marker, base string }
	var bases []repoBase
	for _, name := range budgetRepos {
		cfg := cfgs[name]
		repo := gitwt.New(cfg.SyncDir)
		base, err := repo.Run("rev-parse", "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		addNotes(t, cfg.Path(), map[string]string{proj + "/memory/pending-" + name + ".md": "y"})
		os.Remove(filepath.Join(cfg.SyncDir, ".state", "synced-head"))
		bases = append(bases, repoBase{filepath.Join(cfg.SyncDir, ".state", "hook-markers", "b1"), strings.TrimSpace(base)})
	}

	in := hookJSON("b1", projDir, "UserPromptSubmit")
	var times []time.Duration
	for i := 0; i < 20; i++ {
		for _, b := range bases { // rewind: every run has something to report
			if err := os.MkdirAll(filepath.Dir(b.marker), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := changes.WriteMarker(b.marker, b.base); err != nil {
				t.Fatal(err)
			}
		}
		start := time.Now()
		code, out, errs := runHook(t, in, "--hook", "--deadline", "0")
		times = append(times, time.Since(start))
		if code != 0 || errs != "" {
			t.Fatalf("run %d: exit %d stderr %q", i, code, errs)
		}
		if i == 0 && !strings.Contains(out, "n.md") && !strings.Contains(out, "pending-") {
			t.Fatalf("hook reported nothing:\n%s", out)
		}
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	p95 := times[18]
	t.Logf("B1 hook: min %v median %v p95 %v max %v", times[0], times[10], p95, times[19])
	if p95 >= 200*time.Millisecond {
		t.Errorf("B1: p95 %v, budget 200ms", p95)
	}
}
