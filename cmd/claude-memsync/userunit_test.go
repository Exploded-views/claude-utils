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

// brokenUnit is what kardianos/service generates for a user service: the
// [Install] section names multi-user.target, which a `systemctl --user`
// manager does not have, so the unit never starts at boot.
const brokenUnit = `[Unit]
Description=Synchronizes Claude Code memories
ConditionFileIsExecutable=/usr/local/bin/claude-memsync

[Service]
StartLimitInterval=5
StartLimitBurst=10
ExecStart=/usr/local/bin/claude-memsync "run" "--config" "/home/u/.claudesync/config.json"
Restart=always
RestartSec=120
EnvironmentFile=-/etc/sysconfig/claude-memsync

[Install]
WantedBy=multi-user.target
`

// unitEnv points the unit path at a temp dir, fakes systemctl (recording the
// arguments) and pretends to be Linux.
func unitEnv(t *testing.T, unit string) (path string, calls *[]string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), userUnitName)
	if unit != "" {
		if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var rec []string
	oldP, oldS, oldG, oldI, oldSt := userUnitPathFn, systemctlFn, unitGOOS, kardianosInstall, kardianosStart
	userUnitPathFn = func() (string, error) { return path, nil }
	systemctlFn = func(args ...string) error { rec = append(rec, strings.Join(args, " ")); return nil }
	unitGOOS = "linux"
	t.Cleanup(func() {
		userUnitPathFn, systemctlFn, unitGOOS, kardianosInstall, kardianosStart = oldP, oldS, oldG, oldI, oldSt
	})
	return path, &rec
}

func TestFixUnitTarget_ChangesOnlyTheWantedByLine(t *testing.T) {
	got, changed := fixUnitTarget(brokenUnit)
	if !changed {
		t.Fatal("a multi-user.target unit must be reported as changed")
	}
	want := strings.Replace(brokenUnit, "WantedBy=multi-user.target", "WantedBy=default.target", 1)
	if got != want {
		t.Errorf("only the WantedBy line may change:\n got: %q\nwant: %q", got, want)
	}
	again, changed := fixUnitTarget(got)
	if changed || again != got {
		t.Error("fixing must be idempotent")
	}
}

func TestInstall_FixesAnExistingWrongTargetUnit(t *testing.T) {
	path, calls := unitEnv(t, brokenUnit)
	var out bytes.Buffer
	if err := ensureUserUnitTarget(&out); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "WantedBy=default.target") || strings.Contains(string(b), "multi-user.target") {
		t.Errorf("unit not fixed:\n%s", b)
	}
	// disable first, while the old file still names the old target, so its
	// multi-user.target.wants link is removed; then reload and re-enable.
	want := []string{"disable " + userUnitName, "daemon-reload", "enable " + userUnitName}
	if strings.Join(*calls, "|") != strings.Join(want, "|") {
		t.Errorf("systemctl calls = %v, want %v", *calls, want)
	}
	if n := strings.Count(strings.TrimSpace(out.String()), "\n"); n != 0 || !strings.Contains(out.String(), "default.target") {
		t.Errorf("want one line naming default.target, got %q", out.String())
	}
}

func TestInstallTask_RerunFixesWrongUnitWithoutReinstalling(t *testing.T) {
	path, calls := unitEnv(t, brokenUnit)
	installed := false
	kardianosInstall = func(svc service.Service) error { installed = true; return nil }
	if err := installTask("/x/config.json"); err != nil {
		t.Fatal(err)
	}
	if installed {
		t.Error("kardianos Install refuses an existing unit; a wrong one must be fixed in place")
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "WantedBy=default.target") {
		t.Errorf("unit not fixed:\n%s", b)
	}
	if len(*calls) != 3 || (*calls)[1] != "daemon-reload" {
		t.Errorf("systemctl calls = %v", *calls)
	}
}

func TestStart_FixesWrongTargetThenStarts(t *testing.T) {
	path, calls := unitEnv(t, brokenUnit)
	started := false
	kardianosStart = func(svc service.Service) error { started = true; return nil }
	if err := startTask(); err != nil {
		t.Fatal(err)
	}
	if !started {
		t.Error("start must still start the service")
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "WantedBy=default.target") || len(*calls) != 3 {
		t.Errorf("unit %q, calls %v", b, *calls)
	}
}

func TestEnsureUserUnitTarget_NoOpWhenRightMissingOrNotLinux(t *testing.T) {
	good := strings.Replace(brokenUnit, "multi-user.target", "default.target", 1)
	for name, c := range map[string]struct{ unit, goos string }{
		"already right": {good, "linux"},
		"no unit":       {"", "linux"},
		"darwin":        {brokenUnit, "darwin"},
	} {
		path, calls := unitEnv(t, c.unit)
		unitGOOS = c.goos
		before, _ := os.ReadFile(path)
		var out bytes.Buffer
		if err := ensureUserUnitTarget(&out); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		after, _ := os.ReadFile(path)
		if len(*calls) != 0 || out.Len() != 0 || !bytes.Equal(before, after) {
			t.Errorf("%s: touched things: calls %v out %q", name, *calls, out.String())
		}
	}
}

func TestUnitTargetWarning_NamesTheProblemAndTheFix(t *testing.T) {
	unitEnv(t, brokenUnit)
	w := unitTargetWarning()
	for _, want := range []string{"multi-user.target", "default.target", "claude-memsync install", "systemctl --user"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning lacks %q: %q", want, w)
		}
	}
	unitEnv(t, strings.Replace(brokenUnit, "multi-user.target", "default.target", 1))
	if w := unitTargetWarning(); w != "" {
		t.Errorf("a right unit must not warn: %q", w)
	}
	unitEnv(t, "")
	if w := unitTargetWarning(); w != "" {
		t.Errorf("no unit, no warning: %q", w)
	}
}

func TestStatusAndDoctor_WarnOnWrongTarget(t *testing.T) {
	unitEnv(t, brokenUnit)
	initEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	_, out := captureStdout(t, func() int { return runStatus(nil) })
	if !strings.Contains(out, "multi-user.target") || !strings.Contains(out, "claude-memsync install") {
		t.Errorf("status must warn:\n%s", out)
	}
	var so, se bytes.Buffer
	doctorCmd(&so, &se, []string{"--config", filepath.Join(t.TempDir(), "none.json")})
	if !strings.Contains(so.String(), "multi-user.target") || !strings.Contains(so.String(), "claude-memsync install") {
		t.Errorf("doctor must warn:\n%s", so.String())
	}
}
