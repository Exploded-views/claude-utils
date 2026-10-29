//go:build budget

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/changes"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/testenv"
)

// hookProcFixture is the B1 tree with one pending note in the hook's project
// and every repo's marker rewound, so each `changes --hook` process has
// something to report. rewind puts the markers back.
type hookProcFixture struct {
	ms      string
	stdin   string
	env     []string
	rewind  func()
	tmpPath string
}

func newHookProcFixture(t *testing.T) *hookProcFixture {
	t.Helper()
	cfgs := routedHost(t, budgetRepos, "personal", budgetRoutes())
	ms, _ := testenv.Binaries(t)
	tree := newBudgetTree(t, t.TempDir(), 10000)
	t.Setenv("TMPDIR", tree.tmp)
	proj := tree.real[0]
	notes := map[string]string{}
	for _, f := range tree.real {
		notes[f+"/memory/n.md"] = "x"
	}
	addNotes(t, cfgs["projects"].Path(), notes)
	type repoBase struct{ marker, base string }
	var bases []repoBase
	for _, name := range budgetRepos {
		cfg := cfgs[name]
		base, err := gitwt.New(cfg.SyncDir).Run("rev-parse", "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		addNotes(t, cfg.Path(), map[string]string{proj + "/memory/pending-" + name + ".md": "y"})
		os.Remove(filepath.Join(cfg.SyncDir, ".state", "synced-head"))
		bases = append(bases, repoBase{filepath.Join(cfg.SyncDir, ".state", "hook-markers", "bp"), strings.TrimSpace(base)})
	}
	f := &hookProcFixture{
		ms:    ms,
		stdin: hookJSON("bp", filepath.Join(tree.tmp, "checkout000"), "UserPromptSubmit"),
		env:   os.Environ(),
	}
	f.rewind = func() {
		for _, b := range bases {
			if err := os.MkdirAll(filepath.Dir(b.marker), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := changes.WriteMarker(b.marker, b.base); err != nil {
				t.Fatal(err)
			}
		}
	}
	f.rewind()
	return f
}

type hookRun struct {
	out, errs string
	code      int
	took      time.Duration
}

func (f *hookProcFixture) run(env ...string) hookRun {
	cmd := exec.Command(f.ms, "changes", "--hook")
	cmd.Stdin = strings.NewReader(f.stdin)
	cmd.Env = append(f.env, env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	start := time.Now()
	err := cmd.Run()
	r := hookRun{out: out.String(), errs: errb.String(), took: time.Since(start)}
	if ee, ok := err.(*exec.ExitError); ok {
		r.code = ee.ExitCode()
	}
	return r
}

// B5: 20 hooks started at once: at most one does the work and prints a report;
// the rest skip fast, with no output and exit 0.
func TestBudgetB5_OverlappingHooks(t *testing.T) {
	f := newHookProcFixture(t)
	solo := f.run()
	if !strings.Contains(solo.out, "pending-") {
		t.Fatalf("a lone hook reported nothing: %+v", solo)
	}
	f.rewind()

	const n = 20
	runs := make([]hookRun, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			runs[i] = f.run()
		}()
	}
	t0 := time.Now()
	close(start)
	wg.Wait()
	total := time.Since(t0)
	working, skipped := 0, 0
	var slowestSkip time.Duration
	for _, r := range runs {
		if r.code != 0 {
			t.Errorf("hook exit %d: %q", r.code, r.errs)
		}
		if r.out != "" {
			working++
		} else {
			skipped++
			if r.took > slowestSkip {
				slowestSkip = r.took
			}
		}
	}
	t.Logf("B5 %d hooks at once: %d did the work, %d skipped (slowest skip %v); batch %v; lone hook %v", n, working, skipped, slowestSkip, total, solo.took)
	if working > 1 {
		t.Errorf("B5: %d of %d overlapping hooks did the work, want at most 1", working, n)
	}
}

// B6: a git that hangs does not hang the hook: it prints nothing, exits 0 within
// about 2 s, and leaves no git child running.
func TestBudgetB6_HookDeadline(t *testing.T) {
	f := newHookProcFixture(t)
	bin := t.TempDir()
	pids := filepath.Join(bin, "pids")
	script := "#!/bin/sh\necho $$ >> " + pids + "\nexec sleep 60\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { // stop any stray fake git by its exact PID
		b, _ := os.ReadFile(pids)
		for _, s := range strings.Fields(string(b)) {
			if p, err := strconv.Atoi(s); err == nil {
				_ = syscall.Kill(p, syscall.SIGKILL)
			}
		}
	})
	r := f.run("PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"))
	t.Logf("B6 hook with a hanging git: exit %d after %v, stdout %q", r.code, r.took, r.out)
	if r.code != 0 || r.out != "" {
		t.Errorf("B6: exit %d stdout %q, want 0 and nothing", r.code, r.out)
	}
	if r.took > 2500*time.Millisecond {
		t.Errorf("B6: hook took %v, budget about 2s", r.took)
	}
	time.Sleep(500 * time.Millisecond)
	b, _ := os.ReadFile(pids)
	alive := 0
	for _, s := range strings.Fields(string(b)) {
		if p, err := strconv.Atoi(s); err == nil && syscall.Kill(p, 0) == nil {
			alive++
		}
	}
	if alive > 0 {
		t.Errorf("B6: %d git child(ren) still running after the hook returned", alive)
	}
}
