//go:build !windows

package main

import (
	"bytes"
	"errors"
	"io"
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

// ctlFail, when set, makes the n-th (1-based) call of a systemctl command fail.
var ctlFail func(call string, n int) error

// wantsLink is where `systemctl --user enable` links a default.target unit.
func wantsLink(unit string) string {
	return filepath.Join(filepath.Dir(unit), "default.target.wants", userUnitName)
}

// unitEnv points the unit path at a temp dir, fakes systemctl (recording the
// arguments; enable/disable create/remove the default.target.wants link like
// the real one does) and pretends to be Linux.
func unitEnv(t *testing.T, unit string) (path string, calls *[]string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), userUnitName)
	if unit != "" {
		if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var rec []string
	count := map[string]int{}
	ctlFail = nil
	oldP, oldS, oldG, oldI, oldSt := userUnitPathFn, systemctlFn, unitGOOS, kardianosInstall, kardianosStart
	userUnitPathFn = func() (string, error) { return path, nil }
	systemctlFn = func(args ...string) error {
		call := strings.Join(args, " ")
		rec = append(rec, call)
		count[args[0]]++
		if ctlFail != nil {
			if err := ctlFail(args[0], count[args[0]]); err != nil {
				return err
			}
		}
		switch args[0] {
		case "enable":
			if strings.Contains(brokenOrRight(path), "WantedBy=default.target") {
				os.MkdirAll(filepath.Dir(wantsLink(path)), 0o755)
				os.Symlink(path, wantsLink(path))
			}
		case "disable":
			os.Remove(wantsLink(path))
		}
		return nil
	}
	unitGOOS = "linux"
	t.Cleanup(func() {
		ctlFail = nil
		userUnitPathFn, systemctlFn, unitGOOS, kardianosInstall, kardianosStart = oldP, oldS, oldG, oldI, oldSt
	})
	return path, &rec
}

func brokenOrRight(path string) string { b, _ := os.ReadFile(path); return string(b) }

// confirmed is a unit that is right and enabled: the file says default.target
// and the link exists.
func confirmed(t *testing.T) (path string, calls *[]string) {
	t.Helper()
	path, calls = unitEnv(t, strings.Replace(brokenUnit, "multi-user.target", "default.target", 1))
	if err := os.MkdirAll(filepath.Dir(wantsLink(path)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, wantsLink(path)); err != nil {
		t.Fatal(err)
	}
	return path, calls
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
	want := []string{"daemon-reload", "disable " + userUnitName, "daemon-reload", "enable " + userUnitName}
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
	if len(*calls) != 4 || (*calls)[2] != "daemon-reload" {
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
	if !strings.Contains(string(b), "WantedBy=default.target") || len(*calls) != 4 {
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
		if name == "already right" {
			path, calls = confirmed(t)
		}
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
	confirmed(t)
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

func TestReloadFailsAfterRewrite_StillWarnsAndRetries(t *testing.T) {
	path, calls := unitEnv(t, brokenUnit)
	// the bus probe passes, the reload after the rewrite does not
	ctlFail = func(call string, n int) error {
		if call == "daemon-reload" && n == 2 {
			return errors.New("Failed to connect to bus")
		}
		return nil
	}
	var out bytes.Buffer
	if err := ensureUserUnitTarget(&out); err == nil {
		t.Fatal("a failed reload must be an error")
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "WantedBy=default.target") {
		t.Fatalf("test premise: the file is rewritten:\n%s", b)
	}
	if unitTargetWarning() == "" {
		t.Error("file says default.target but nothing is enabled: status and doctor must still warn")
	}
	ctlFail = nil
	*calls = nil
	if err := ensureUserUnitTarget(&out); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if unitTargetWarning() != "" {
		t.Error("after the retry the unit is confirmed; no warning")
	}
	if got := strings.Join(*calls, "|"); got != "daemon-reload|daemon-reload|enable "+userUnitName {
		t.Errorf("retry calls = %s", got)
	}
}

func TestBusUnreachable_ChangesNothingAndPrintsManualSteps(t *testing.T) {
	path, calls := unitEnv(t, brokenUnit)
	ctlFail = func(call string, n int) error { return errors.New("Failed to connect to bus") }
	var out bytes.Buffer
	if err := ensureUserUnitTarget(&out); err == nil {
		t.Fatal("an unreachable bus must be an error")
	}
	if b, _ := os.ReadFile(path); string(b) != brokenUnit {
		t.Errorf("the unit must be untouched:\n%s", b)
	}
	if len(*calls) != 1 || (*calls)[0] != "daemon-reload" {
		t.Errorf("only the probe may run: %v", *calls)
	}
	for _, want := range []string{"systemctl --user disable", "default.target", "systemctl --user daemon-reload", "systemctl --user enable"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("manual steps lack %q:\n%s", want, out.String())
		}
	}
}

func TestWarn_MissingOrDanglingWantsLink(t *testing.T) {
	path, _ := confirmed(t)
	if w := unitTargetWarning(); w != "" {
		t.Fatalf("premise: confirmed unit warns: %q", w)
	}
	os.Remove(wantsLink(path))
	if unitTargetWarning() == "" {
		t.Error("a missing wants link must warn")
	}
	if err := os.Symlink(filepath.Join(filepath.Dir(path), "gone.service"), wantsLink(path)); err != nil {
		t.Fatal(err)
	}
	if unitTargetWarning() == "" {
		t.Error("a dangling wants link must warn")
	}
	os.Remove(wantsLink(path))
	other := filepath.Join(filepath.Dir(path), "other.service")
	os.WriteFile(other, []byte("x"), 0o644)
	os.Symlink(other, wantsLink(path))
	if unitTargetWarning() == "" {
		t.Error("a wants link to some other file must warn")
	}
}

func TestFixUnitTarget_OnlyTheInstallSectionLineExactly(t *testing.T) {
	const odd = "[Unit]\n# WantedBy=multi-user.target\nWantedBy=multi-user.target\n[Install]\n# WantedBy=multi-user.target\nWantedBy=multi-user.target\n"
	got, changed := fixUnitTarget(odd)
	want := "[Unit]\n# WantedBy=multi-user.target\nWantedBy=multi-user.target\n[Install]\n# WantedBy=multi-user.target\nWantedBy=default.target\n"
	if !changed || got != want {
		t.Errorf("got %q (changed %v), want %q", got, changed, want)
	}
	for _, multi := range []string{
		"[Install]\nWantedBy=multi-user.target graphical.target\n",
		"[Install]\nWantedBy=multi-user.target extra\n",
		"[Install]\n  # WantedBy=multi-user.target\n",
	} {
		if g, c := fixUnitTarget(multi); c || g != multi {
			t.Errorf("must leave %q alone, got %q (changed %v)", multi, g, c)
		}
	}
	path, _ := unitEnv(t, "[Unit]\nDescription=x\n[Install]\nWantedBy=multi-user.target graphical.target\n")
	var out bytes.Buffer
	if err := ensureUserUnitTarget(&out); err == nil {
		t.Error("a multi-value line cannot be repaired automatically: error, not success")
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "multi-user.target graphical.target") {
		t.Errorf("the line must be left as it is:\n%s", b)
	}
	if unitTargetWarning() == "" {
		t.Error("and it must warn")
	}
}

func TestRepair_RefusesASymlinkedUnit(t *testing.T) {
	path, _ := unitEnv(t, "")
	real := filepath.Join(filepath.Dir(path), "real.service")
	os.WriteFile(real, []byte(brokenUnit), 0o644)
	if err := os.Symlink(real, path); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := ensureUserUnitTarget(&out)
	if err == nil || !strings.Contains(err.Error()+out.String(), "symlink") {
		t.Errorf("want a symlink refusal, got %v / %q", err, out.String())
	}
	if st, _ := os.Lstat(path); st.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink must not be replaced")
	}
	if b, _ := os.ReadFile(real); string(b) != brokenUnit {
		t.Error("the link target must be untouched")
	}
}

func TestRepair_KeepsTheFileMode(t *testing.T) {
	path, _ := unitEnv(t, brokenUnit)
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := ensureUserUnitTarget(io.Discard); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640 (a temp file is 0600, so 0640 catches a lost chmod)", st.Mode().Perm())
	}
	ents, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".claude-memsync-unit-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func execLine(t *testing.T, path string) string {
	t.Helper()
	b, _ := os.ReadFile(path)
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "ExecStart=") {
			return l
		}
	}
	return ""
}

func TestInstallRerun_NewArgsRewriteExecStart(t *testing.T) {
	path, calls := confirmed(t)
	installed := false
	kardianosInstall = func(svc service.Service) error { installed = true; return nil }
	exe, _ := os.Executable()
	want := execStartLine(exe, []string{"run", "--config", "/new/config.json", "--routes", "/new/routes.json"})
	old := unitRoutes
	unitRoutes = "/new/routes.json"
	oc, or := installCfgExplicit, installRoutesExplicit
	installCfgExplicit, installRoutesExplicit = true, true // both flags were given
	t.Cleanup(func() { unitRoutes, installCfgExplicit, installRoutesExplicit = old, oc, or })
	var err error
	stderr := captureStderr(t, func() { err = installTask("/new/config.json") })
	if err != nil {
		t.Fatal(err)
	}
	if installed {
		t.Error("kardianos Install must not run over an existing unit")
	}
	if got := execLine(t, path); got != want {
		t.Errorf("ExecStart = %q, want %q", got, want)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "WantedBy=default.target") || !strings.Contains(string(b), "RestartSec=120") {
		t.Errorf("the other lines must stay:\n%s", b)
	}
	if !strings.Contains(stderr, "ExecStart") || !strings.Contains(stderr, "restart") {
		t.Errorf("say that the arguments changed and a restart applies them: %q", stderr)
	}
	if len(*calls) == 0 || (*calls)[0] != "daemon-reload" {
		t.Errorf("calls = %v", *calls)
	}
}

func TestInstallRerun_SameArgsAndRightUnitKeepsKardianosBehaviour(t *testing.T) {
	path, calls := confirmed(t)
	exe, _ := os.Executable()
	b, _ := os.ReadFile(path)
	cur := strings.Replace(string(b), execLine(t, path), execStartLine(exe, []string{"run", "--config", "/x/config.json"}), 1)
	os.WriteFile(path, []byte(cur), 0o644)
	installed := false
	kardianosInstall = func(svc service.Service) error { installed = true; return nil }
	if err := installTask("/x/config.json"); err != nil {
		t.Fatal(err)
	}
	if !installed || len(*calls) != 0 {
		t.Errorf("nothing to repair: installed=%v calls=%v", installed, *calls)
	}
}
