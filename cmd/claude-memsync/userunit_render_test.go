//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kardianos/service"
)

// Renders the unit with the real kardianos template. systemctl is a recording
// script on a PATH that holds nothing else, so no real systemd is reached.
func TestGeneratedUserUnit_InstallSectionNamesDefaultTarget(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd units are Linux only")
	}
	bin := t.TempDir()
	log := filepath.Join(bin, "calls.log")
	script := "#!/bin/sh\necho \"$@\" >> '" + log + "'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// kardianos picks systemd when rpm-ostree is on the PATH (or PID 1 is
	// systemd, which a build container is not).
	if err := os.WriteFile(filepath.Join(bin, "rpm-ostree"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Registered before t.Setenv so it runs after the PATH is restored.
	all := service.AvailableSystems()
	t.Cleanup(func() { service.ChooseSystem(all...) })
	home := t.TempDir()
	t.Setenv("PATH", bin)
	t.Setenv("HOME", home)
	// kardianos decided its system at start-up; choose systemd again now that
	// the PATH says so.
	for _, sys := range all {
		if strings.Contains(sys.String(), "systemd") {
			service.ChooseSystem(sys)
		}
	}
	svc, err := userSvc("/x/config.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(svc.Platform(), "systemd") {
		t.Skipf("not a systemd host (%s)", svc.Platform())
	}
	unit := filepath.Join(home, ".config/systemd/user", userUnitName)
	oldP, oldS := userUnitPathFn, systemctlFn
	userUnitPathFn = func() (string, error) { return unit, nil }
	systemctlFn = func(args ...string) error {
		f, _ := os.OpenFile(log, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
		defer f.Close()
		f.WriteString("ours --user " + strings.Join(args, " ") + "\n")
		if args[0] == "enable" { // what the real one does
			os.MkdirAll(filepath.Dir(wantsLink(unit)), 0o755)
			os.Symlink(unit, wantsLink(unit))
		}
		return nil
	}
	oldG := unitGOOS
	unitGOOS = "linux"
	t.Cleanup(func() { userUnitPathFn, systemctlFn, unitGOOS = oldP, oldS, oldG })

	if err := installTask("/x/config.json"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(unit)
	if err != nil {
		t.Fatal(err)
	}
	install := string(b)[strings.Index(string(b), "[Install]"):]
	if strings.TrimSpace(install) != "[Install]\nWantedBy=default.target" {
		t.Errorf("[Install] section = %q", install)
	}
	if strings.Contains(string(b), "multi-user.target") {
		t.Errorf("unit still names multi-user.target:\n%s", b)
	}
}

// A launchd user agent must start at login (RunAtLoad) and be restarted if it
// dies (KeepAlive). kardianos defaults RunAtLoad to false, so the plist of a
// default install would not start the daemon at login.
func TestUserOptions_LaunchdRunsAtLoadAndKeepsAlive(t *testing.T) {
	opts := userOptions()
	if v, _ := opts["RunAtLoad"].(bool); !v {
		t.Errorf("RunAtLoad must be true for a login agent: %v", opts)
	}
	if v, ok := opts["KeepAlive"]; ok && v != true {
		t.Errorf("KeepAlive must not be disabled: %v", opts)
	}
	if v, _ := opts["UserService"].(bool); !v {
		t.Errorf("UserService must stay true: %v", opts)
	}
}
