package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/foreign"
	"github.com/MarimerLLC/claude-utils/internal/route"
	"github.com/kardianos/service"

	syncpkg "github.com/MarimerLLC/claude-utils/internal/sync"
)

func quietMarkerFor(cfg config.Config) string { return quietMarker(cfg.SyncDir) }

// fakeHost replaces the real systemctl and /proc for a test and makes the quiet
// window instant unless the test sets onWait.
type fakeHost struct {
	units  map[string]string // unit -> is-enabled output
	noSys  bool              // systemctl cannot be run
	procs  []string
	onWait func()
	waited []time.Duration
	mu     sync.Mutex
}

// set changes the fake's state under h.mu, which the daemon's goroutines
// read it under.
func (h *fakeHost) set(f func()) {
	h.mu.Lock()
	f()
	h.mu.Unlock()
}

// setUnit changes a unit's state; the daemon's goroutines read it under h.mu.
func (h *fakeHost) setUnit(unit, state string) {
	h.mu.Lock()
	h.units[unit] = state
	h.mu.Unlock()
}

func useFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	h := &fakeHost{units: map[string]string{}}
	oldEnv, oldWait := foreignEnv, quietWait
	foreignEnv = func() foreign.Env {
		return foreign.Env{
			GOOS: "linux",
			Home: os.Getenv("HOME"),
			Systemctl: func(args ...string) (string, error) {
				h.mu.Lock()
				noSys := h.noSys
				h.mu.Unlock()
				if noSys {
					return "", errors.New("systemctl: not found")
				}
				if args[1] == "is-enabled" {
					h.mu.Lock()
					defer h.mu.Unlock()
					return h.units[args[2]], nil
				}
				return "inactive", nil
			},
			Processes: func() ([]string, error) {
				h.mu.Lock()
				defer h.mu.Unlock()
				return append([]string(nil), h.procs...), nil
			},
			Logf: func(string, ...any) {},
		}
	}
	quietWait = func(d time.Duration) {
		h.mu.Lock()
		h.waited = append(h.waited, d)
		h.mu.Unlock()
		if h.onWait != nil {
			h.onWait()
		}
	}
	t.Cleanup(func() { foreignEnv, quietWait = oldEnv, oldWait })
	return h
}

func setForeign(t *testing.T, cfg config.Config, ws ...foreign.Writer) config.Config {
	t.Helper()
	cfg.ForeignWriters = ws
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func singleHost(t *testing.T) config.Config {
	t.Helper()
	initEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv(routesEnv, "")
	cfg := config.Defaults()
	cfg.SyncDir = filepath.Join(home, ".claudesync")
	cfg.ClaudeProjectsDir = filepath.Join(home, ".claude", "projects")
	return cfg
}

func TestConfigRoundTripsForeignWritersAndQuietSec(t *testing.T) {
	cfg := singleHost(t)
	zero := 0
	cfg.StartQuietSec = &zero
	cfg = setForeign(t, cfg, foreign.Writer{SystemdUnit: "old.timer"})
	got, err := config.Load(cfg.Path())
	if err != nil || len(got.ForeignWriters) != 1 || got.QuietSec() != 0 {
		t.Fatalf("%v %+v quiet=%d", err, got.ForeignWriters, got.QuietSec())
	}
	if d := config.Defaults(); d.QuietSec() != 15 {
		t.Errorf("default quiet window is 15s, got %d", d.QuietSec())
	}
	// A malformed entry is refused at load, not ignored.
	os.WriteFile(cfg.Path(), []byte(`{"foreignWriters":[{"file":"/a","process":"p"}]}`), 0o600)
	if _, err := config.Load(cfg.Path()); err == nil {
		t.Error("ambiguous foreignWriters entry must be refused")
	}
}

func TestRoutesJSONCarriesForeignWriters(t *testing.T) {
	p := filepath.Join(t.TempDir(), "routes.json")
	os.WriteFile(p, []byte(`{"repos":{},"routes":[],"foreignWriters":[{"process":"old-sync"}]}`), 0o600)
	f, err := route.Load(p)
	if err != nil || len(f.ForeignWriters) != 1 || f.ForeignWriters[0].Process != "old-sync" {
		t.Fatalf("%v %+v", err, f.ForeignWriters)
	}
	os.WriteFile(p, []byte(`{"repos":{},"routes":[],"foreignWriters":[{}]}`), 0o600)
	if _, err := route.Load(p); err == nil {
		t.Error("an empty entry must be refused")
	}
}

func TestStartGuard_SingleRepoRefusesActiveWriter(t *testing.T) {
	h := useFakeHost(t)
	cfg := singleHost(t)
	setForeign(t, cfg, foreign.Writer{SystemdUnit: "old-sync.timer"})
	h.setUnit("old-sync.timer", "enabled")
	err := startGuard(cfg.Path(), "", false, guardOpts{})
	if err == nil || !strings.Contains(err.Error(), "foreign writer systemdUnit old-sync.timer") {
		t.Fatalf("want a refusal naming the entry: %v", err)
	}
	if strings.Count(err.Error(), "\n") != 0 {
		t.Errorf("one line please: %q", err)
	}
	if len(h.waited) != 0 {
		t.Error("a refused start must not sit through the quiet window")
	}
	// --ignore-foreign-writers overrides.
	if err := startGuard(cfg.Path(), "", false, guardOpts{IgnoreForeign: true}); err != nil {
		t.Fatalf("override: %v", err)
	}
	// Disabled: starts.
	h.setUnit("old-sync.timer", "disabled")
	if err := startGuard(cfg.Path(), "", false, guardOpts{}); err != nil {
		t.Fatal(err)
	}
}

func TestStartGuard_NoListStillRunsQuietWindow(t *testing.T) {
	h := useFakeHost(t)
	cfg := singleHost(t)
	cfg = setForeign(t, cfg)
	p := filepath.Join(cfg.ClaudeProjectsDir, "f1", "memory", "n.md")
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, []byte("a"), 0o600)
	n := 0
	h.onWait = func() { n++; os.WriteFile(p, []byte(strings.Repeat("b", n+1)), 0o600) }
	err := startGuard(cfg.Path(), "", false, guardOpts{})
	if err == nil || !strings.Contains(err.Error(), "owned folder changed during start-up quiet window") {
		t.Fatalf("%v", err)
	}
	if len(h.waited) != 1 || h.waited[0] != 15*time.Second {
		t.Errorf("waited %v", h.waited)
	}
	// The override covers the list, never the quiet window.
	if err := startGuard(cfg.Path(), "", false, guardOpts{IgnoreForeign: true}); err == nil {
		t.Error("--ignore-foreign-writers must not skip the quiet window")
	}
	// startQuietSec 0 disables it.
	zero := 0
	cfg.StartQuietSec = &zero
	config.Save(cfg)
	h.waited = nil
	if err := startGuard(cfg.Path(), "", false, guardOpts{}); err != nil || len(h.waited) != 0 {
		t.Fatalf("%v waited %v", err, h.waited)
	}
}

func TestStartGuard_RoutedUsesRoutesListAndOwnedFolders(t *testing.T) {
	h := useFakeHost(t)
	cfgs := routedHost(t, []string{"personal", "projects"}, "personal", []map[string]string{{"folder": "mine", "repo": "projects"}})
	rp := os.Getenv(routesEnv)
	b, _ := os.ReadFile(rp)
	var rj map[string]any
	json.Unmarshal(b, &rj)
	rj["foreignWriters"] = []foreign.Writer{{Process: "old-sync.py"}}
	b, _ = json.Marshal(rj)
	os.WriteFile(rp, b, 0o600)

	h.set(func() { h.procs = []string{"python3 /x/old-sync.py"} })
	err := startGuard("", rp, true, guardOpts{})
	if err == nil || !strings.Contains(err.Error(), "foreign writer process old-sync.py") {
		t.Fatalf("%v", err)
	}
	h.set(func() { h.procs = nil })
	// Quiet window: a folder owned by "projects" changing refuses; one nobody owns does not.
	claude := cfgs["projects"].ClaudeProjectsDir
	mine := filepath.Join(claude, "mine", "memory", "n.md")
	other := filepath.Join(claude, "unrouted", "memory", "n.md")
	for _, p := range []string{mine, other} {
		os.MkdirAll(filepath.Dir(p), 0o700)
		os.WriteFile(p, []byte("a"), 0o600)
	}
	h.onWait = func() { os.WriteFile(other, []byte("zzz"), 0o600) }
	if err := startGuard("", rp, true, guardOpts{}); err != nil {
		t.Fatalf("an unowned folder changing is not our concern: %v", err)
	}
	h.onWait = func() { os.WriteFile(mine, []byte("zzz"), 0o600) }
	err = startGuard("", rp, true, guardOpts{ForceQuiet: true})
	if err == nil || !strings.Contains(err.Error(), "quiet window") || strings.Contains(err.Error(), "mine") {
		t.Fatalf("%v", err)
	}
}

func TestStatus_ShowsBlockedForeignWriter(t *testing.T) {
	h := useFakeHost(t)
	cfg := singleHost(t)
	setForeign(t, cfg, foreign.Writer{SystemdUnit: "old-sync.timer"})
	h.setUnit("old-sync.timer", "enabled")
	old := statusFn
	statusFn = func() (string, error) { return "", service.ErrNotInstalled }
	t.Cleanup(func() { statusFn = old })
	// loadDistillConfig("") reads the default config path under HOME.
	code, out := captureStdout(t, func() int { return runStatus(nil) })
	if !strings.Contains(out, "blocked: foreign writer systemdUnit old-sync.timer") || code == 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	h.setUnit("old-sync.timer", "disabled")
	code, out = captureStdout(t, func() int { return runStatus(nil) })
	if strings.Contains(out, "blocked") || code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

// init reads the list from an existing config.json of the sync dir (a
// re-init), or from the routes file when there is one.
func TestInit_RefusesActiveWriterBeforeAnythingIsCloned(t *testing.T) {
	h := useFakeHost(t)
	cfg, origin := initFixture(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv(routesEnv, "")
	h.setUnit("old-sync.timer", "enabled")
	setForeign(t, cfg, foreign.Writer{SystemdUnit: "old-sync.timer"})

	err := initGuard(cfg, nil, guardOpts{})
	if err == nil || !strings.Contains(err.Error(), "foreign writer systemdUnit old-sync.timer") {
		t.Fatalf("%v", err)
	}
	if err := initGuard(cfg, nil, guardOpts{IgnoreForeign: true}); err != nil {
		t.Fatalf("override: %v", err)
	}
	if ents, _ := os.ReadDir(filepath.Join(origin, "refs", "heads")); len(ents) != 0 {
		t.Error("origin must be untouched")
	}
}

func TestInit_RoutesFileListApplies(t *testing.T) {
	h := useFakeHost(t)
	cfg, _ := initFixture(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	rp := filepath.Join(home, "routes.json")
	os.WriteFile(rp, []byte(`{"repos":{},"routes":[],"foreignWriters":[{"process":"old-sync.py"}]}`), 0o600)
	t.Setenv(routesEnv, rp)
	h.set(func() { h.procs = []string{"python3 old-sync.py"} })
	if err := initGuard(cfg, nil, guardOpts{}); err == nil || !strings.Contains(err.Error(), "foreign writer process old-sync.py") {
		t.Fatalf("%v", err)
	}
}

func TestInit_QuietWindowRefusesAChangingFolder(t *testing.T) {
	h := useFakeHost(t)
	cfg, _ := initFixture(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv(routesEnv, "")
	p := filepath.Join(cfg.ClaudeProjectsDir, "p1", "memory", "ok.md")
	h.onWait = func() { os.WriteFile(p, []byte("changed by someone else"), 0o600) }
	err := initGuard(cfg, nil, guardOpts{})
	if err == nil || !strings.Contains(err.Error(), "quiet window") {
		t.Fatalf("%v", err)
	}
}

// ---- review round: fail closed, markers, re-init, wiring, runtime hold ----

func TestStartGuard_UncheckableUnitRefusesOnLinux(t *testing.T) {
	h := useFakeHost(t)
	cfg := singleHost(t)
	setForeign(t, cfg, foreign.Writer{SystemdUnit: "old-sync.timer"})
	h.set(func() { h.noSys = true })
	err := startGuard(cfg.Path(), "", false, guardOpts{})
	if err == nil || !strings.Contains(err.Error(), "could not be checked") {
		t.Fatalf("%v", err)
	}
	if err := startGuard(cfg.Path(), "", false, guardOpts{IgnoreForeign: true}); err != nil {
		t.Fatalf("override: %v", err)
	}
}

func TestStartGuard_QuietWindowOnlyOnFirstStart(t *testing.T) {
	h := useFakeHost(t)
	cfg := singleHost(t)
	setForeign(t, cfg)
	p := filepath.Join(cfg.ClaudeProjectsDir, "f1", "memory", "n.md")
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, []byte("a"), 0o600)
	if err := startGuard(cfg.Path(), "", false, guardOpts{}); err != nil || len(h.waited) != 1 {
		t.Fatalf("first start waits: %v %v", err, h.waited)
	}
	if _, err := os.Stat(quietMarkerFor(cfg)); err != nil {
		t.Fatalf("marker: %v", err)
	}
	// A busy host: the folder changes all the time, later starts do not care.
	n := 0
	h.onWait = func() { n++; os.WriteFile(p, []byte(strings.Repeat("x", n+5)), 0o600) }
	h.waited = nil
	if err := startGuard(cfg.Path(), "", false, guardOpts{}); err != nil || len(h.waited) != 0 {
		t.Fatalf("later start skips the window: %v %v", err, h.waited)
	}
	// --quiet-window forces it.
	if err := startGuard(cfg.Path(), "", false, guardOpts{ForceQuiet: true}); err == nil || len(h.waited) != 1 {
		t.Fatalf("forced window must run and see the writer: %v %v", err, h.waited)
	}
}

func TestStartGuard_SkipQuietWindowIsDeliberateAndMarks(t *testing.T) {
	h := useFakeHost(t)
	cfg := singleHost(t)
	setForeign(t, cfg)
	if err := startGuard(cfg.Path(), "", false, guardOpts{SkipQuiet: true}); err != nil || len(h.waited) != 0 {
		t.Fatalf("%v %v", err, h.waited)
	}
	if _, err := os.Stat(quietMarkerFor(cfg)); err != nil {
		t.Error("a deliberate first start also counts as the first start")
	}
	if err := startGuard(cfg.Path(), "", false, guardOpts{SkipQuiet: true, ForceQuiet: true}); err == nil {
		t.Error("the two flags contradict each other")
	}
}

func TestStartGuard_RefusedWindowLeavesNoMarker(t *testing.T) {
	h := useFakeHost(t)
	cfg := singleHost(t)
	setForeign(t, cfg)
	p := filepath.Join(cfg.ClaudeProjectsDir, "f1", "memory", "n.md")
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, []byte("a"), 0o600)
	h.onWait = func() { os.WriteFile(p, []byte("bbb"), 0o600) }
	if err := startGuard(cfg.Path(), "", false, guardOpts{}); err == nil {
		t.Fatal("want refusal")
	}
	if _, err := os.Stat(quietMarkerFor(cfg)); err == nil {
		t.Error("a refused window must not count as the first start")
	}
}

func TestStartGuard_RoutedMarksEachRepoAndSkipsDone(t *testing.T) {
	h := useFakeHost(t)
	cfgs := routedHost(t, []string{"personal", "projects"}, "personal", nil)
	rp := os.Getenv(routesEnv)
	if err := startGuard("", rp, true, guardOpts{}); err != nil || len(h.waited) != 1 {
		t.Fatalf("%v %v", err, h.waited)
	}
	for n, c := range cfgs {
		if _, err := os.Stat(quietMarker(c.SyncDir)); err != nil {
			t.Errorf("%s: no marker", n)
		}
	}
	os.Remove(quietMarker(cfgs["projects"].SyncDir)) // a repo added later
	h.waited = nil
	if err := startGuard("", rp, true, guardOpts{}); err != nil || len(h.waited) != 1 {
		t.Fatalf("the new repo gets its window: %v %v", err, h.waited)
	}
}

func TestBootstrap_ClearsQuietMarkerAndCarriesTheListOver(t *testing.T) {
	useFakeHost(t)
	cfg, _ := initFixture(t)
	zero := 3
	old := cfg
	old.ForeignWriters = []foreign.Writer{{SystemdUnit: "old-sync.timer"}}
	old.StartQuietSec = &zero
	if err := config.Save(old); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(quietMarker(cfg.SyncDir)), 0o700)
	os.WriteFile(quietMarker(cfg.SyncDir), []byte("x"), 0o600)
	fresh := cfg // what runInit builds: no list
	if err := bootstrapFull(fresh, true, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	got, err := config.Load(cfg.Path())
	if err != nil || len(got.ForeignWriters) != 1 || got.ForeignWriters[0].SystemdUnit != "old-sync.timer" || got.QuietSec() != 3 {
		t.Fatalf("re-init lost the guard config: %v %+v quiet=%d", err, got.ForeignWriters, got.QuietSec())
	}
	if _, err := os.Stat(quietMarker(cfg.SyncDir)); err == nil {
		t.Error("init must reset the first-start marker")
	}
}

func TestInitGuard_BrokenRoutesFileRefuses(t *testing.T) {
	useFakeHost(t)
	cfg, _ := initFixture(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	rp := filepath.Join(home, "routes.json")
	os.WriteFile(rp, []byte(`{broken`), 0o600)
	t.Setenv(routesEnv, rp)
	if err := initGuard(cfg, nil, guardOpts{}); err == nil {
		t.Fatal("a routes.json that exists but will not load must refuse init")
	}
}

// runInit and runService must actually call the guard.
func TestRunInit_RefusesAnActiveWriterBeforeTouchingTheRemote(t *testing.T) {
	h := useFakeHost(t)
	cfg, origin := initFixture(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv(routesEnv, "")
	h.setUnit("old-sync.timer", "enabled")
	setForeign(t, cfg, foreign.Writer{SystemdUnit: "old-sync.timer"})
	exe, _ := os.Executable()
	drv := filepath.Join(filepath.Dir(exe), "claude-memmerge")
	if _, err := os.Stat(drv); err != nil {
		if werr := os.WriteFile(drv, []byte("#!/bin/sh\n"), 0o755); werr != nil {
			t.Skip("cannot place merge driver")
		}
		t.Cleanup(func() { os.Remove(drv) })
	}
	args := []string{"--remote", origin, "--sync-dir", cfg.SyncDir, "--claude-dir", cfg.ClaudeProjectsDir, "--force"}
	code, _ := captureErr(t, func() int { return runInit(args) })
	if code == 0 {
		t.Fatal("init must refuse")
	}
	if ents, _ := os.ReadDir(filepath.Join(origin, "refs", "heads")); len(ents) != 0 {
		t.Errorf("origin received refs: %v", ents)
	}
	if _, err := os.Stat(filepath.Join(cfg.SyncDir, ".git")); err == nil {
		t.Error("nothing may be cloned or initialised")
	}
	// With the override it goes ahead and pushes.
	code, errs := captureErr(t, func() int { return runInit(append(args, "--ignore-foreign-writers")) })
	if code != 0 {
		t.Fatalf("override: %d %s", code, errs)
	}
	if ents, _ := os.ReadDir(filepath.Join(origin, "refs", "heads")); len(ents) == 0 {
		t.Error("the override should let init push")
	}
}

func TestRunService_RefusesBeforeAnyServiceStarts(t *testing.T) {
	h := useFakeHost(t)
	cfg := singleHost(t)
	setForeign(t, cfg, foreign.Writer{SystemdUnit: "old-sync.timer"})
	h.setUnit("old-sync.timer", "enabled")
	code, errs := captureErr(t, func() int { return runService(cfg.Path(), "", guardOpts{}) })
	if code != 1 || !strings.Contains(errs, "foreign writer systemdUnit old-sync.timer") {
		t.Fatalf("code %d stderr %q", code, errs)
	}
	if _, err := os.Stat(pidFilePathIn(cfg.SyncDir)); err == nil {
		t.Error("no PID file before the guard passes")
	}
	if code := runRun([]string{"--config", cfg.Path(), "--ignore-foreign-writers", "--skip-quiet-window", "--quiet-window"}); code == 0 {
		t.Error("contradicting flags must fail")
	}
}

func captureErr(t *testing.T, f func() int) (int, string) {
	t.Helper()
	ef, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = ef
	code := f()
	os.Stderr = old
	b, _ := os.ReadFile(ef.Name())
	return code, string(b)
}

func TestRouted_ForeignWriterHoldsOnlyThatRepoAndRechecks(t *testing.T) {
	h := useFakeHost(t)
	cfgs := routedHost(t, []string{"personal", "projects", "other"}, "personal", nil)
	setForeign(t, cfgs["projects"], foreign.Writer{SystemdUnit: "old-sync.timer"})
	set, err := loadRouted(os.Getenv(routesEnv))
	if err != nil {
		t.Fatal(err)
	}
	set.recheckForeign()
	if set.held("projects") || set.held("other") {
		t.Fatal("nothing active yet")
	}
	h.setUnit("old-sync.timer", "enabled")
	set.recheckForeign()
	if !set.held("projects") || set.held("other") || set.held("personal") {
		t.Fatalf("only the repo that lists the writer is held")
	}
	// Sticky: the writer going away does not lift the hold, because what it
	// left behind would be mirrored. Only a restart does.
	h.setUnit("old-sync.timer", "disabled")
	set.recheckForeign()
	if !set.held("projects") {
		t.Fatal("the hold must stay until a daemon restart")
	}
	if b, err := os.ReadFile(foreignHeldPath(cfgs["projects"].SyncDir)); err != nil || !strings.Contains(string(b), "restart after cleanup") {
		t.Errorf("held marker for status: %q %v", b, err)
	}
	if _, err := os.Stat(foreignHeldPath(cfgs["other"].SyncDir)); err == nil {
		t.Error("only the held repo gets a marker")
	}
	set2, _ := loadRouted(os.Getenv(routesEnv))
	set2.recheckForeign()
	if set2.held("projects") {
		t.Fatal("a fresh daemon with the writer gone resumes")
	}
	if _, err := os.Stat(foreignHeldPath(cfgs["projects"].SyncDir)); err == nil {
		t.Error("a restart clears the marker")
	}
	h.setUnit("old-sync.timer", "enabled")
	set = set2
	// A flaky systemctl keeps the previous verdict instead of flipping it.
	set.recheckForeign()
	h.set(func() { h.noSys = true })
	set.recheckForeign()
	if !set.held("projects") {
		t.Error("an unanswerable check must not release a hold")
	}
	// ...and an unanswerable check never sets one.
	set3, _ := loadRouted(os.Getenv(routesEnv))
	set3.recheckForeign()
	if set3.held("projects") {
		t.Error("an unanswerable check must not hold a repo")
	}
}

func TestRouted_HoldLogsOnce(t *testing.T) {
	h := useFakeHost(t)
	cfgs := routedHost(t, []string{"projects"}, "", nil)
	setForeign(t, cfgs["projects"], foreign.Writer{SystemdUnit: "old-sync.timer"})
	h.setUnit("old-sync.timer", "enabled")
	set, _ := loadRouted(os.Getenv(routesEnv))
	var buf strings.Builder
	set.logf = func(f string, a ...any) { fmt.Fprintf(&buf, f+"\n", a...) }
	h.setUnit("old-sync.timer", "enabled")
	set.recheckForeign()
	set.recheckForeign()
	set.recheckForeign()
	if n := strings.Count(buf.String(), "restart after cleanup"); n != 1 {
		t.Errorf("want one log line, got %d:\n%s", n, buf.String())
	}
}

func TestRouted_StartWiresHoldAndReloadAndRestartRecheck(t *testing.T) {
	h := useFakeHost(t)
	cfgs := routedHost(t, []string{"projects"}, "", nil)
	setForeign(t, cfgs["projects"], foreign.Writer{SystemdUnit: "old-sync.timer"})
	set, _ := loadRouted(os.Getenv(routesEnv))
	r, err := set.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()
	if len(r.loops) != 1 || r.loops[0].Hold == nil {
		t.Fatal("each loop needs a Hold")
	}
	if r.loops[0].Hold() {
		t.Fatal("not held while the writer is off")
	}
	// A supervised restart rechecks.
	h.setUnit("old-sync.timer", "enabled")
	// A fake loop supervised beside the running set: no package variable is
	// assigned while the set's goroutines read them.
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	fake := func(c context.Context, l *syncpkg.Loop) error {
		calls++
		if calls >= 2 {
			cancel()
		}
		return errors.New("died")
	}
	superviseLoopWith(ctx, r.loops[0], func() { set.recheckForeign() }, fake, time.Millisecond)
	if !r.loops[0].Hold() {
		t.Error("a loop restart must recheck the foreign writers")
	}
	// A routes reload rechecks too.
	h.setUnit("old-sync.timer", "disabled")
	set.reload()
	if !r.loops[0].Hold() {
		t.Error("a reload does not lift a hold either")
	}
}

func TestStatus_RoutedShowsHeldRepo(t *testing.T) {
	h := useFakeHost(t)
	cfgs := routedHost(t, []string{"personal", "projects"}, "personal", nil)
	setForeign(t, cfgs["projects"], foreign.Writer{SystemdUnit: "old-sync.timer"})
	h.setUnit("old-sync.timer", "enabled")
	old := statusFn
	statusFn = func() (string, error) { return "", service.ErrNotInstalled }
	t.Cleanup(func() { statusFn = old })
	code, out := captureStdout(t, func() int { return runStatus(nil) })
	if code == 0 || !strings.Contains(out, "[projects] blocked: foreign writer systemdUnit old-sync.timer") || strings.Contains(out, "[personal] blocked") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	// A daemon held it: status keeps saying so after the writer is gone.
	set, _ := loadRouted(os.Getenv(routesEnv))
	set.recheckForeign()
	h.setUnit("old-sync.timer", "disabled")
	code, out = captureStdout(t, func() int { return runStatus(nil) })
	if code == 0 || !strings.Contains(out, "[projects] held: foreign writer seen: restart after cleanup") || strings.Contains(out, "[personal] held") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

// fastQuietWindow keeps runInit tests from sitting through the real 15 s.
func fastQuietWindow(t *testing.T) {
	t.Helper()
	old := quietWait
	quietWait = func(time.Duration) {}
	t.Cleanup(func() { quietWait = old })
}
