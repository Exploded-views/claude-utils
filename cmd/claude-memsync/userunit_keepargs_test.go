//go:build !windows

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kardianos/service"
)

const keepExec = `ExecStart=/old/bin/claude-memsync "run" "--config" "/keep/config.json" "--routes" "/keep/routes.json"`

// liveUnit is a confirmed (default.target + wants link) unit with keepExec.
func liveUnit(t *testing.T, execLine string) (path string, calls *[]string) {
	t.Helper()
	unit := strings.Replace(brokenUnit, "multi-user.target", "default.target", 1)
	unit = strings.Replace(unit, `ExecStart=/usr/local/bin/claude-memsync "run" "--config" "/home/u/.claudesync/config.json"`, execLine, 1)
	path, calls = unitEnv(t, unit)
	if err := os.MkdirAll(filepath.Dir(wantsLink(path)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, wantsLink(path)); err != nil {
		t.Fatal(err)
	}
	return path, calls
}

func restoreUnitRoutes(t *testing.T) {
	old := unitRoutes
	t.Cleanup(func() { unitRoutes = old })
}

func TestInstallBare_KeepsExistingConfigAndRoutes(t *testing.T) {
	restoreUnitRoutes(t)
	routedHost(t, []string{"personal", "projects"}, "personal", nil)
	path, _ := liveUnit(t, keepExec)
	kardianosInstall = func(service.Service) error { return nil }
	var code int
	errs := captureStderr(t, func() { code = runInstall(nil) })
	if code != 0 {
		t.Fatalf("install exit %d: %s", code, errs)
	}
	got := execLine(t, path)
	if !strings.HasSuffix(got, ` "run" "--config" "/keep/config.json" "--routes" "/keep/routes.json"`) {
		t.Errorf("a bare install must keep both arguments, got %q", got)
	}
}

func TestInstallRoutesFlag_ReplacesOnlyRoutesAndPrintsOldAndNew(t *testing.T) {
	restoreUnitRoutes(t)
	routedHost(t, []string{"personal", "projects"}, "personal", nil)
	path, _ := liveUnit(t, keepExec)
	kardianosInstall = func(service.Service) error { return nil }
	b, err := os.ReadFile(os.Getenv(routesEnv))
	if err != nil {
		t.Fatal(err)
	}
	z := filepath.Join(t.TempDir(), "z-routes.json")
	if err := os.WriteFile(z, b, 0o600); err != nil {
		t.Fatal(err)
	}
	var code int
	errs := captureStderr(t, func() { code = runInstall([]string{"--routes", z}) })
	if code != 0 {
		t.Fatalf("install exit %d: %s", code, errs)
	}
	got := execLine(t, path)
	if !strings.Contains(got, `"--config" "/keep/config.json"`) || !strings.HasSuffix(got, `"--routes" "`+z+`"`) {
		t.Errorf("only routes may change, got %q", got)
	}
	if !strings.Contains(errs, keepExec) {
		t.Errorf("the old ExecStart line must be printed:\n%s", errs)
	}
	if !strings.Contains(errs, got) {
		t.Errorf("the new ExecStart line must be printed:\n%s", errs)
	}
	if !strings.Contains(errs, "restart") {
		t.Errorf("tell the user to restart:\n%s", errs)
	}
}

func TestInstallRerun_CRLFUnitEqualIsNotRewritten(t *testing.T) {
	restoreUnitRoutes(t)
	unitRoutes = ""
	exe, _ := os.Executable()
	line := execStartLine(exe, []string{"run", "--config", "/x/config.json"})
	path, calls := liveUnit(t, line)
	b, _ := os.ReadFile(path)
	crlf := strings.ReplaceAll(string(b), "\n", "\r\n")
	if err := os.WriteFile(path, []byte(crlf), 0o644); err != nil {
		t.Fatal(err)
	}
	installed := false
	kardianosInstall = func(service.Service) error { installed = true; return nil }
	if err := installTask("/x/config.json"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != crlf || len(*calls) != 0 {
		t.Errorf("an equal CRLF unit must not be rewritten (calls %v):\n%q", *calls, after)
	}
	if !installed {
		t.Error("nothing to repair: the kardianos path runs as before")
	}
}

func TestSetExecStart_OnlyInsideService(t *testing.T) {
	const want = `ExecStart=/new "run"`
	in := "[Unit]\nExecStart=/bogus\n\n[Install]\nExecStart=/bogus2\nWantedBy=default.target\n"
	if got, changed := setExecStart(in, want); changed || got != in {
		t.Errorf("ExecStart outside [Service] must be ignored: %v %q", changed, got)
	}
	in2 := "[Unit]\nExecStart=/bogus\n\n[Service]\nExecStart=/real \"run\"\n"
	got, changed := setExecStart(in2, want)
	if !changed || !strings.Contains(got, "[Unit]\nExecStart=/bogus\n") || !strings.Contains(got, "[Service]\n"+want+"\n") {
		t.Errorf("only the [Service] line changes: %v %q", changed, got)
	}
	// CRLF: an equal line is not a change, and a replaced line keeps its CR.
	if _, changed := setExecStart("[Service]\r\n"+want+"\r\n", want); changed {
		t.Error("an equal CRLF line is not a change")
	}
	if got, _ := setExecStart("[Service]\r\nExecStart=/old\r\n", want); !strings.Contains(got, want+"\r\n") {
		t.Errorf("CRLF must be kept: %q", got)
	}
}

func TestUnreadableUnit_WarnsEverywhere(t *testing.T) {
	path, calls := unitEnv(t, "")
	if err := os.Mkdir(path, 0o755); err != nil { // reading a directory fails with an error other than ErrNotExist
		t.Fatal(err)
	}
	for name, w := range map[string]string{"doctor/status": unitTargetWarning()} {
		if !strings.Contains(w, "cannot read the service unit (") || !strings.Contains(w, "); autostart not confirmed") {
			t.Errorf("%s: %q", name, w)
		}
	}
	var out bytes.Buffer
	_ = ensureUserUnitTarget(&out)
	if !strings.Contains(out.String(), "cannot read the service unit (") || !strings.Contains(out.String(), "autostart not confirmed") {
		t.Errorf("repair must warn: %q", out.String())
	}
	if errs := captureStderr(t, func() { _ = installTask("/x/config.json") }); !strings.Contains(errs, "cannot read the service unit") {
		t.Errorf("install must warn: %q", errs)
	}
	if len(*calls) != 0 {
		t.Errorf("nothing may be changed: %v", *calls)
	}
}

func TestUnitTargetWarning_MissingUnitIsSilent(t *testing.T) {
	unitEnv(t, "")
	if w := unitTargetWarning(); w != "" {
		t.Errorf("an absent unit is not a read problem: %q", w)
	}
}

func TestRepair_SymlinkedUnitIsRefusedBeforeDisable(t *testing.T) {
	path, calls := unitEnv(t, "")
	real := filepath.Join(filepath.Dir(path), "real.service")
	if err := os.WriteFile(real, []byte(brokenUnit), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(wantsLink(path)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, wantsLink(path)); err != nil { // enabled
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := ensureUserUnitTarget(&out); err == nil || !strings.Contains(err.Error()+out.String(), "symlink") {
		t.Errorf("want a symlink refusal, got %v / %q", err, out.String())
	}
	for _, c := range *calls {
		if strings.HasPrefix(c, "disable") {
			t.Errorf("disable must never run on a symlinked unit: %v", *calls)
		}
	}
	if _, err := os.Lstat(wantsLink(path)); err != nil {
		t.Error("the unit must stay enabled")
	}
	if b, _ := os.ReadFile(real); string(b) != brokenUnit {
		t.Error("the link target must be untouched")
	}
}
