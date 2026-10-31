package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/proc"
	syncpkg "github.com/MarimerLLC/claude-utils/internal/sync"
)

// realisticConfigs gives every config what `init` always writes (mergeDriverPath)
// plus a token command, saved 0600: the configs a real host has, which make
// loadConfig run the git trust check. The budget fixtures built them from
// config.Defaults(), which has neither, so that check never ran there.
func realisticConfigs(t *testing.T, cfgs map[string]config.Config) {
	t.Helper()
	for name, cfg := range cfgs {
		cfg.MergeDriverPath = "/opt/bin/claude-memmerge"
		cfg.GitHubTokenCmd = []string{"/bin/echo", "t"}
		if err := config.Save(cfg); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(cfg.Path(), 0o600); err != nil {
			t.Fatal(err)
		}
		keepConfigOut(t, cfg)
		cfgs[name] = cfg
	}
}

// requireTrusted fails unless every config passes the trust check, so a
// fixture really is the case where a sensitive key is honoured.
func requireTrusted(t *testing.T, cfgs map[string]config.Config) {
	t.Helper()
	for name, cfg := range cfgs {
		raw, err := config.Load(cfg.Path())
		if err != nil {
			t.Fatal(err)
		}
		if err := configTrusted(raw); err != nil {
			t.Fatalf("repo %s: fixture config is not trusted (%v): the trust check would drop its keys", name, err)
		}
	}
}

// realisticHost is routedHost for n repos r00.. whose configs are realistic;
// repo rNN owns the folder f-rNN.
func realisticHost(t *testing.T, n int) (map[string]config.Config, []string) {
	t.Helper()
	var names []string
	var routes []map[string]string
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("r%02d", i)
		names = append(names, name)
		routes = append(routes, map[string]string{"folder": "f-" + name, "repo": name})
	}
	cfgs := routedHost(t, names, "", routes)
	realisticConfigs(t, cfgs)
	return cfgs, names
}

// noteFor commits a note into repo name's folder; the body makes it unique.
func noteFor(t *testing.T, cfgs map[string]config.Config, name, tag string) string {
	t.Helper()
	file := "n-" + name + "-" + tag + ".md"
	addNotes(t, cfgs[name].Path(), map[string]string{"f-" + name + "/memory/" + file: tag})
	return file
}

// shimCounter counts every git process started through PATH.
type shimCounter struct{ calls string }

func (s *shimCounter) reset() { os.Remove(s.calls) }
func (s *shimCounter) count() int {
	b, _ := os.ReadFile(s.calls)
	return strings.Count(string(b), "x\n")
}

// gitShim puts a git on PATH (with only /usr/bin:/bin behind it) that logs each
// start and, if delay is set, sleeps that long first (sleep's argument).
func gitShim(t *testing.T, delay string) *shimCounter {
	t.Helper()
	pre := ""
	if delay != "" {
		pre = "sleep " + delay + "\n"
	}
	return gitShimScript(t, pre)
}

// gitShimScript is gitShim with pre, shell run after the start is logged and
// before the real git (it sees the git arguments as "$@" and the last one in
// $last).
func gitShimScript(t *testing.T, pre string) *shimCounter {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	bin := t.TempDir()
	s := &shimCounter{calls: filepath.Join(bin, "calls")}
	body := "#!/bin/sh\necho x >> " + s.calls + "\nlast=\nfor a in \"$@\"; do last=$a; done\n" + pre
	body += "exec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	return s
}

// countedHook runs the hook once and reports the git processes it started, as
// the proc seam saw them and as the PATH shim saw them (they must agree).
func countedHook(t *testing.T, s *shimCounter, in string, args ...string) (seam, shimmed int, out, errs string) {
	t.Helper()
	var n atomic.Int64
	restore := proc.SetSpawnHook(func(string, []string) { n.Add(1) })
	defer restore()
	s.reset()
	_, out, errs = runHook(t, in, append([]string{"--hook"}, args...)...)
	return int(n.Load()), s.count(), out, errs
}

func requireSpawns(t *testing.T, what string, seam, shimmed, want int) {
	t.Helper()
	if seam != want || shimmed != want {
		t.Errorf("%s: %d git processes through the proc seam, %d seen by the PATH shim, want %d", what, seam, shimmed, want)
	}
}

// fileHead is the repo's HEAD read from its loose branch ref: no git process.
func fileHead(cfg config.Config) string {
	b, _ := os.ReadFile(filepath.Join(cfg.SyncDir, ".git", "refs", "heads", "main"))
	return strings.TrimSpace(string(b))
}

func markerOf(cfg config.Config, session string) string {
	b, _ := os.ReadFile(filepath.Join(cfg.SyncDir, ".state", "hook-markers", session))
	return strings.TrimSpace(string(b))
}

// workerSync makes the next hook's worker signal when it ends, so a test can
// wait for a worker that outlived the deadline before the next call.
func workerSync(t *testing.T) (wait func()) {
	t.Helper()
	done := make(chan struct{}, 64)
	hookWorkDone = func() { done <- struct{}{} }
	t.Cleanup(func() { hookWorkDone = nil })
	return func() {
		t.Helper()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("the hook's worker never ended")
		}
	}
}

// A hook on a host whose configs carry mergeDriverPath and a token command (as
// init writes them) starts no git when nothing is new, at 40 repos, and a
// brand-new session's first look (a baseline) starts none either. The trust
// check used to run 4 git processes per repo per load, and the hook loaded the
// configs several times.
func TestHook_RealisticConfigsSpawnNoGitWhenNothingNew(t *testing.T) {
	cfgs, names := realisticHost(t, 40)
	for _, n := range names {
		noteFor(t, cfgs, n, "base")
	}
	requireTrusted(t, cfgs)
	s := gitShim(t, "")
	in := hookJSON("s-a0", "", "UserPromptSubmit")
	seam, shimmed, out, errs := countedHook(t, s, in, "--deadline", "0")
	requireSpawns(t, "a new session's first look at 40 repos", seam, shimmed, 0)
	if out != "" || errs != "" {
		t.Errorf("first look printed %q / %q, want nothing", out, errs)
	}
	for _, n := range names {
		if got, want := markerOf(cfgs[n], "s-a0"), fileHead(cfgs[n]); got == "" || got != want {
			t.Fatalf("repo %s: marker %q, want the baseline %q", n, got, want)
		}
	}
	seam, shimmed, out, errs = countedHook(t, s, in, "--deadline", "0")
	requireSpawns(t, "a repeat prompt with nothing new", seam, shimmed, 0)
	if out != "" || errs != "" {
		t.Errorf("repeat prompt printed %q / %q, want nothing", out, errs)
	}
}

// Each config is loaded once per hook: the prelude's load is the worker's too.
func TestHook_LoadsEachConfigOnce(t *testing.T) {
	cfgs, names := realisticHost(t, 3)
	for _, n := range names {
		noteFor(t, cfgs, n, "base")
	}
	in := hookJSON("s-once", "", "UserPromptSubmit")
	runHook(t, in, "--hook", "--deadline", "0") // baseline
	var mu sync.Mutex
	loads := map[string]int{}
	cfgLoadHook = func(p string) { mu.Lock(); loads[p]++; mu.Unlock() }
	t.Cleanup(func() { cfgLoadHook = nil })
	runHook(t, in, "--hook", "--deadline", "0")
	for _, n := range names {
		if got := loads[cfgs[n].Path()]; got != 1 {
			t.Errorf("repo %s: config loaded %d times in one hook, want 1", n, got)
		}
	}
}

// The held-repo alerts depend on nothing slow: even when every git process is
// slow (a loaded machine) the prelude is done long before the deadline, so the
// alerts of a worker stuck later still go out.
func TestHook_PreludeIsGitFree(t *testing.T) {
	cfgs, names := realisticHost(t, 40)
	s := gitShim(t, "0.03")
	writeRecord(t, cfgs[names[5]].Path(), "2026-10-26T02:50:29Z "+heldMarker+"check failed: HTTP 404\n")
	release := make(chan struct{})
	wait := workerSync(t)
	hookTestHook = func() { <-release }
	t.Cleanup(func() { hookTestHook = nil })
	in := hookJSON("s-pre", "", "UserPromptSubmit")
	seam, shimmed, out, errs := countedHook(t, s, in, "--deadline", "300ms")
	close(release)
	wait()
	if !strings.Contains(errs, "deadline exceeded") {
		t.Fatalf("stderr %q, want the deadline path", errs)
	}
	if !strings.Contains(hookContext(t, out), "is HELD") {
		t.Errorf("the held alert was lost to a slow prelude; stdout %q", out)
	}
	if _, err := os.Stat(filepath.Join(cfgs[names[5]].SyncDir, ".state", "hook-markers", "s-pre.held")); err != nil {
		t.Errorf("the alert went out but its .held marker did not move: %v", err)
	}
	requireSpawns(t, "the prelude of a stuck worker", seam, shimmed, 0)
}

// The first look of a session costs no git process, and its baseline is the
// synced head when that is behind HEAD (a note in HEAD is not on disk yet).
func TestHook_FirstUseBaselineIsGitFree(t *testing.T) {
	cfgs, names := realisticHost(t, 3)
	for _, n := range names {
		noteFor(t, cfgs, n, "base")
	}
	s := gitShim(t, "")

	t.Run("no synced head", func(t *testing.T) {
		in := hookJSON("s-b1", "", "UserPromptSubmit")
		seam, shimmed, out, _ := countedHook(t, s, in, "--deadline", "0")
		requireSpawns(t, "first use", seam, shimmed, 0)
		if out != "" {
			t.Errorf("a baseline printed %q", out)
		}
		for _, n := range names {
			if markerOf(cfgs[n], "s-b1") != fileHead(cfgs[n]) {
				t.Errorf("repo %s: marker %q, want HEAD", n, markerOf(cfgs[n], "s-b1"))
			}
		}
		file := noteFor(t, cfgs, names[1], "news")
		_, _, out, _ = countedHook(t, s, in, "--deadline", "0")
		if got := strings.Count(hookContext(t, out), file); got != 1 {
			t.Errorf("the next call reported %q %d times:\n%s", file, got, out)
		}
	})

	t.Run("synced head at HEAD", func(t *testing.T) {
		for _, n := range names {
			if err := syncpkg.MoveSyncedHead(gitwt.New(cfgs[n].SyncDir), "", fileHead(cfgs[n])); err != nil {
				t.Fatal(err)
			}
		}
		seam, shimmed, _, _ := countedHook(t, s, hookJSON("s-b2", "", "UserPromptSubmit"), "--deadline", "0")
		requireSpawns(t, "first use with the synced head at HEAD", seam, shimmed, 0)
	})

	t.Run("synced head behind HEAD", func(t *testing.T) {
		n := names[0]
		behind := fileHead(cfgs[n])
		if err := syncpkg.MoveSyncedHead(gitwt.New(cfgs[n].SyncDir), behind, behind); err != nil {
			t.Fatal(err)
		}
		file := noteFor(t, cfgs, n, "unsynced") // HEAD is now ahead of the record
		for _, o := range names[1:] {
			if err := syncpkg.MoveSyncedHead(gitwt.New(cfgs[o].SyncDir), fileHead(cfgs[o]), fileHead(cfgs[o])); err != nil {
				t.Fatal(err)
			}
		}
		in := hookJSON("s-b3", "", "UserPromptSubmit")
		seam, shimmed, out, _ := countedHook(t, s, in, "--deadline", "0")
		if seam > 1 || shimmed > 1 {
			t.Errorf("first use with one repo behind: %d / %d git processes, want at most 1", seam, shimmed)
		}
		if out != "" {
			t.Errorf("a baseline printed %q", out)
		}
		if got := markerOf(cfgs[n], "s-b3"); got != behind {
			t.Errorf("marker %q, want the synced head %q (not HEAD: the note would be lost)", got, behind)
		}
		if err := syncpkg.MoveSyncedHead(gitwt.New(cfgs[n].SyncDir), behind, fileHead(cfgs[n])); err != nil {
			t.Fatal(err)
		}
		_, _, out, _ = countedHook(t, s, in, "--deadline", "0")
		if got := strings.Count(hookContext(t, out), file); got != 1 {
			t.Errorf("after the copy, %q was reported %d times:\n%s", file, got, out)
		}
	})

	t.Run("busy repo", func(t *testing.T) {
		if err := os.MkdirAll(filepath.Join(cfgs[names[0]].SyncDir, ".git", "rebase-merge"), 0o700); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Remove(filepath.Join(cfgs[names[0]].SyncDir, ".git", "rebase-merge")) })
		countedHook(t, s, hookJSON("s-b4", "", "UserPromptSubmit"), "--deadline", "0")
		if got := markerOf(cfgs[names[0]], "s-b4"); got != "" {
			t.Errorf("a repo in the middle of a rebase got a marker %q", got)
		}
		if got := markerOf(cfgs[names[1]], "s-b4"); got == "" {
			t.Error("the other repos got no baseline")
		}
	})
}

// pendingRepos is how many repos have a marker that is not at their HEAD.
func pendingRepos(cfgs map[string]config.Config, names []string, session string) int {
	n := 0
	for _, name := range names {
		if markerOf(cfgs[name], session) != fileHead(cfgs[name]) {
			n++
		}
	}
	return n
}

// With news in 40 repos and only 5 able to finish before a short deadline, each
// call still settles the repos that finished: their notices are printed once
// and their markers move, so the next call spends its time on the rest. Today
// a call that overruns throws everything away, every time.
func TestHook_DeadlineProgressConverges(t *testing.T) {
	const n = 40
	cfgs, names := realisticHost(t, n)
	in := hookJSON("s-conv", "", "UserPromptSubmit")
	runHook(t, in, "--hook", "--deadline", "0") // baselines
	notes := map[string]string{}
	for _, name := range names {
		notes[name] = noteFor(t, cfgs, name, "news")
	}
	wait := workerSync(t)
	t.Cleanup(func() { hookRepoTestHook = nil })
	printed := map[string]int{}
	calls := 0
	for ; pendingRepos(cfgs, names, "s-conv") > 0 && calls < 12; calls++ {
		var quota atomic.Int32
		quota.Store(5)
		release := make(chan struct{})
		hookRepoTestHook = func(name string) {
			if markerOf(cfgs[name], "s-conv") == fileHead(cfgs[name]) {
				return // settled earlier: free
			}
			if quota.Add(-1) < 0 {
				<-release
			}
		}
		before := map[string]string{}
		for _, name := range names {
			before[name] = markerOf(cfgs[name], "s-conv")
		}
		pending := pendingRepos(cfgs, names, "s-conv")
		_, out, errs := runHook(t, in, "--hook", "--deadline", "200ms")
		close(release)
		wait()
		ctx := hookContext(t, out)
		for _, name := range names {
			if got := strings.Count(ctx, notes[name]); got > 0 {
				printed[name] += got
			}
			moved := markerOf(cfgs[name], "s-conv") != before[name]
			if moved && !strings.Contains(ctx, notes[name]) {
				t.Errorf("call %d: repo %s moved its marker but its notice was not printed (lost)", calls+1, name)
			}
			if !moved && strings.Contains(ctx, notes[name]) {
				t.Errorf("call %d: repo %s printed its notice but its marker did not move (it would repeat)", calls+1, name)
			}
		}
		if after := pendingRepos(cfgs, names, "s-conv"); after >= pending {
			t.Fatalf("call %d settled nothing (%d repos still pending); stderr %q", calls+1, after, errs)
		}
	}
	for _, name := range names {
		if printed[name] != 1 {
			t.Errorf("repo %s: its notice was printed %d times over %d calls, want 1", name, printed[name], calls)
		}
	}
	if calls > n/5+1 {
		t.Errorf("took %d calls, want at most %d", calls, n/5+1)
	}
	hookRepoTestHook = nil
	s := gitShim(t, "")
	seam, shimmed, out, _ := countedHook(t, s, in, "--deadline", "0")
	requireSpawns(t, "the call after convergence", seam, shimmed, 0)
	if out != "" {
		t.Errorf("the call after convergence printed %q", out)
	}
}

// The deadline reply carries the held alerts first, then the notices of the
// repos that finished, and moves exactly those markers (and the .held one).
func TestHook_PartialReplyCarriesAlertsAndFinishedRepos(t *testing.T) {
	cfgs, names := realisticHost(t, 4)
	in := hookJSON("s-part", "", "UserPromptSubmit")
	runHook(t, in, "--hook", "--deadline", "0")
	notes := map[string]string{}
	for _, name := range names {
		notes[name] = noteFor(t, cfgs, name, "news")
	}
	writeRecord(t, cfgs["r03"].Path(), "2026-10-26T02:50:29Z "+heldMarker+"check failed: HTTP 404\n")
	base := map[string]string{}
	for _, name := range names {
		base[name] = markerOf(cfgs[name], "s-part")
	}
	wait := workerSync(t)
	release := make(chan struct{})
	hookRepoTestHook = func(name string) {
		if name != "r00" {
			<-release
		}
	}
	t.Cleanup(func() { hookRepoTestHook = nil })
	_, out, errs := runHook(t, in, "--hook", "--deadline", "200ms")
	close(release)
	wait()
	ctx := hookContext(t, out)
	if !strings.Contains(errs, "hook deadline exceeded; notices deferred to the next prompt") {
		t.Errorf("stderr %q lost the deadline line", errs)
	}
	a, f := strings.Index(ctx, "is HELD"), strings.Index(ctx, framingLine)
	if a < 0 || f < 0 || a > f {
		t.Errorf("want the held alert, then the notices:\n%s", ctx)
	}
	if !strings.Contains(ctx, notes["r00"]) {
		t.Errorf("the finished repo's notice is missing:\n%s", ctx)
	}
	for _, name := range names[1:] {
		if strings.Contains(ctx, notes[name]) {
			t.Errorf("repo %s did not finish but its notice was printed", name)
		}
		if got := markerOf(cfgs[name], "s-part"); got != base[name] {
			t.Errorf("repo %s did not finish but its marker moved to %q (late finisher or over-eager commit)", name, got)
		}
	}
	if got := markerOf(cfgs["r00"], "s-part"); got != fileHead(cfgs["r00"]) {
		t.Errorf("the finished repo's marker is %q, want HEAD", got)
	}
	if _, err := os.Stat(filepath.Join(cfgs["r03"].SyncDir, ".state", "hook-markers", "s-part.held")); err != nil {
		t.Errorf("the alert was printed but its .held marker did not move: %v", err)
	}
}

// If the partial reply cannot be written, nothing moves: not the finished
// repos' markers and not the .held marker, so the next prompt says it all.
func TestHook_PartialReplyWriteFailureMovesNothing(t *testing.T) {
	cfgs, names := realisticHost(t, 2)
	in := hookJSON("s-wf", "", "UserPromptSubmit")
	runHook(t, in, "--hook", "--deadline", "0")
	for _, name := range names {
		noteFor(t, cfgs, name, "news")
	}
	writeRecord(t, cfgs["r01"].Path(), "2026-10-26T02:50:29Z "+heldMarker+"check failed: HTTP 404\n")
	base := markerOf(cfgs["r00"], "s-wf")
	wait := workerSync(t)
	release := make(chan struct{})
	hookRepoTestHook = func(name string) {
		if name != "r00" {
			<-release
		}
	}
	t.Cleanup(func() { hookRepoTestHook = nil })
	var errb strings.Builder
	changesHook(strings.NewReader(in), failWriter{}, &errb, "", "", "", 20, 200*time.Millisecond)
	close(release)
	wait()
	if got := markerOf(cfgs["r00"], "s-wf"); got != base {
		t.Errorf("the reply failed but the marker moved from %q to %q", base, got)
	}
	if _, err := os.Stat(filepath.Join(cfgs["r01"].SyncDir, ".state", "hook-markers", "s-wf.held")); err == nil {
		t.Error("the reply failed but the .held marker moved")
	}
}

// Repos that finish after the deadline move no marker, not even a baseline.
func TestHook_LateFinishersMoveNoMarker(t *testing.T) {
	cfgs, names := realisticHost(t, 3)
	in := hookJSON("s-late", "", "UserPromptSubmit")
	runHook(t, in, "--hook", "--deadline", "0")
	for _, name := range names {
		noteFor(t, cfgs, name, "news")
	}
	wait := workerSync(t)
	release := make(chan struct{})
	hookRepoTestHook = func(name string) {
		if name != "r00" {
			<-release
		}
	}
	t.Cleanup(func() { hookRepoTestHook = nil })
	base := map[string]string{"r01": markerOf(cfgs["r01"], "s-late"), "r02": markerOf(cfgs["r02"], "s-late")}
	runHook(t, in, "--hook", "--deadline", "200ms")
	close(release)
	wait() // the late repos have now run to the end
	if got := markerOf(cfgs["r00"], "s-late"); got != fileHead(cfgs["r00"]) {
		t.Errorf("the finished repo's marker is %q, want HEAD", got)
	}
	for _, name := range []string{"r01", "r02"} {
		if got := markerOf(cfgs[name], "s-late"); got != base[name] {
			t.Errorf("repo %s finished after the deadline but its marker is %q, want %q", name, got, base[name])
		}
	}
}

// At most 8 repos are worked on at once: unbounded, all finish together and a
// deadline cuts every one of them.
func TestHook_RepoParallelismIsBounded(t *testing.T) {
	_, _ = realisticHost(t, 40)
	in := hookJSON("s-cap", "", "UserPromptSubmit")
	runHook(t, in, "--hook", "--deadline", "0")
	var entered atomic.Int32
	var mu sync.Mutex
	var first []string
	release := make(chan struct{})
	hookRepoTestHook = func(name string) {
		mu.Lock()
		first = append(first, name)
		mu.Unlock()
		entered.Add(1)
		<-release
	}
	t.Cleanup(func() { hookRepoTestHook = nil })
	finished := make(chan struct{})
	go func() { runHook(t, in, "--hook", "--deadline", "0"); close(finished) }()
	for start := time.Now(); entered.Load() < 8 && time.Since(start) < 20*time.Second; {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // time for any more to get in
	got := entered.Load()
	mu.Lock()
	snap := append([]string(nil), first...) // what got in before the release
	mu.Unlock()
	close(release)
	<-finished
	if got != 8 {
		t.Errorf("%d repos were worked on at once, want exactly 8", got)
	}
	// The slots go to the first repos in target order, so the ones that finish
	// first form a prefix a deadline can keep.
	for _, name := range snap {
		if name > "r07" {
			t.Errorf("repo %s got a slot before r00..r07 did: %v", name, snap)
			break
		}
	}
}
