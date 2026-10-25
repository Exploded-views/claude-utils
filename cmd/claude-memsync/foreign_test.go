package main

import (
	"encoding/json"
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
)

// fakeHost replaces the real systemctl and /proc for a test and makes the quiet
// window instant unless the test sets onWait.
type fakeHost struct {
	units  map[string]string // unit -> is-enabled output
	procs  []string
	onWait func()
	waited []time.Duration
	mu     sync.Mutex
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
				if args[1] == "is-enabled" {
					return h.units[args[2]], nil
				}
				return "inactive", nil
			},
			Processes: func() ([]string, error) { return h.procs, nil },
			Logf:      func(string, ...any) {},
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
	h.units["old-sync.timer"] = "enabled"
	err := startGuard(cfg.Path(), "", false, false)
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
	if err := startGuard(cfg.Path(), "", false, true); err != nil {
		t.Fatalf("override: %v", err)
	}
	// Disabled: starts.
	h.units["old-sync.timer"] = "disabled"
	if err := startGuard(cfg.Path(), "", false, false); err != nil {
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
	err := startGuard(cfg.Path(), "", false, false)
	if err == nil || !strings.Contains(err.Error(), "owned folder changed during start-up quiet window") {
		t.Fatalf("%v", err)
	}
	if len(h.waited) != 1 || h.waited[0] != 15*time.Second {
		t.Errorf("waited %v", h.waited)
	}
	// The override covers the list, never the quiet window.
	if err := startGuard(cfg.Path(), "", false, true); err == nil {
		t.Error("--ignore-foreign-writers must not skip the quiet window")
	}
	// startQuietSec 0 disables it.
	zero := 0
	cfg.StartQuietSec = &zero
	config.Save(cfg)
	h.waited = nil
	if err := startGuard(cfg.Path(), "", false, false); err != nil || len(h.waited) != 0 {
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

	h.procs = []string{"python3 /x/old-sync.py"}
	err := startGuard("", rp, true, false)
	if err == nil || !strings.Contains(err.Error(), "foreign writer process old-sync.py") {
		t.Fatalf("%v", err)
	}
	h.procs = nil
	// Quiet window: a folder owned by "projects" changing refuses; one nobody owns does not.
	claude := cfgs["projects"].ClaudeProjectsDir
	mine := filepath.Join(claude, "mine", "memory", "n.md")
	other := filepath.Join(claude, "unrouted", "memory", "n.md")
	for _, p := range []string{mine, other} {
		os.MkdirAll(filepath.Dir(p), 0o700)
		os.WriteFile(p, []byte("a"), 0o600)
	}
	h.onWait = func() { os.WriteFile(other, []byte("zzz"), 0o600) }
	if err := startGuard("", rp, true, false); err != nil {
		t.Fatalf("an unowned folder changing is not our concern: %v", err)
	}
	h.onWait = func() { os.WriteFile(mine, []byte("zzz"), 0o600) }
	err = startGuard("", rp, true, false)
	if err == nil || !strings.Contains(err.Error(), "quiet window") || strings.Contains(err.Error(), "mine") {
		t.Fatalf("%v", err)
	}
}

func TestStatus_ShowsBlockedForeignWriter(t *testing.T) {
	h := useFakeHost(t)
	cfg := singleHost(t)
	setForeign(t, cfg, foreign.Writer{SystemdUnit: "old-sync.timer"})
	h.units["old-sync.timer"] = "enabled"
	old := statusFn
	statusFn = func() (string, error) { return "", service.ErrNotInstalled }
	t.Cleanup(func() { statusFn = old })
	// loadDistillConfig("") reads the default config path under HOME.
	code, out := captureStdout(t, func() int { return runStatus(nil) })
	if !strings.Contains(out, "blocked: foreign writer systemdUnit old-sync.timer") || code == 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	h.units["old-sync.timer"] = "disabled"
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
	h.units["old-sync.timer"] = "enabled"
	setForeign(t, cfg, foreign.Writer{SystemdUnit: "old-sync.timer"})

	err := initGuard(cfg, nil, false)
	if err == nil || !strings.Contains(err.Error(), "foreign writer systemdUnit old-sync.timer") {
		t.Fatalf("%v", err)
	}
	if err := initGuard(cfg, nil, true); err != nil {
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
	h.procs = []string{"python3 old-sync.py"}
	if err := initGuard(cfg, nil, false); err == nil || !strings.Contains(err.Error(), "foreign writer process old-sync.py") {
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
	err := initGuard(cfg, nil, false)
	if err == nil || !strings.Contains(err.Error(), "quiet window") {
		t.Fatalf("%v", err)
	}
}
