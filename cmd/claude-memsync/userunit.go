package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// kardianos/service writes `WantedBy=multi-user.target` into every systemd
// unit it generates, user units included. A `systemctl --user` manager has no
// multi-user.target, so such a unit shows "enabled" yet never starts at boot.
// A user unit must be WantedBy=default.target. The service option
// SystemdScript could replace the template, but that copies all of it (and the
// template functions are unexported); the unit is fixed after the write
// instead, which keeps every other line as kardianos generated it.

const (
	userUnitName  = "claude-memsync.service"
	wrongWantedBy = "WantedBy=multi-user.target"
	rightWantedBy = "WantedBy=default.target"
)

// unitGOOS is runtime.GOOS; tests replace it.
var unitGOOS = runtime.GOOS

// userUnitPathFn is the systemd user unit's path; tests replace it.
var userUnitPathFn = func() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", userUnitName), nil
}

// systemctlFn runs `systemctl --user <args>`; tests replace it.
var systemctlFn = func(args ...string) error {
	out, err := exec.Command("systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl --user %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// fixUnitTarget points the [Install] WantedBy of a user unit at
// default.target. Only that line changes; changed is false when there is
// nothing to do, so the fix is idempotent.
func fixUnitTarget(content string) (string, bool) {
	if !strings.Contains(content, wrongWantedBy) {
		return content, false
	}
	return strings.ReplaceAll(content, wrongWantedBy, rightWantedBy), true
}

// userUnitWrong reads the installed unit and reports whether it has the wrong
// target. A missing unit, a non-Linux host or an unreadable file is "no".
func userUnitWrong() (path, content string, wrong bool) {
	if unitGOOS != "linux" {
		return "", "", false
	}
	p, err := userUnitPathFn()
	if err != nil {
		return "", "", false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", "", false
	}
	_, changed := fixUnitTarget(string(b))
	return p, string(b), changed
}

// ensureUserUnitTarget repairs an installed user unit that is WantedBy
// multi-user.target: it disables the unit while the old file still names the
// old target (so the multi-user.target.wants link goes), rewrites the file
// atomically, reloads and enables it again (default.target.wants). A unit that
// is right, absent, or not on Linux is left alone and nothing is printed.
func ensureUserUnitTarget(w io.Writer) error {
	path, content, wrong := userUnitWrong()
	if !wrong {
		return nil
	}
	fixed, _ := fixUnitTarget(content)
	_ = systemctlFn("disable", userUnitName) // best effort: the unit may not be enabled
	if err := writeFileAtomic(path, []byte(fixed)); err != nil {
		return fmt.Errorf("fix %s: %w", path, err)
	}
	if err := systemctlFn("daemon-reload"); err != nil {
		return err
	}
	if err := systemctlFn("enable", userUnitName); err != nil {
		return err
	}
	fmt.Fprintf(w, "fixed autostart: %s is now WantedBy=default.target (was multi-user.target, which a user manager never reaches); reloaded and re-enabled\n", userUnitName)
	return nil
}

func writeFileAtomic(path string, data []byte) error {
	mode := fs.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".claude-memsync-unit-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, err = tmp.Write(data)
	err = errors.Join(err, tmp.Chmod(mode), tmp.Close())
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		os.Remove(name)
	}
	return err
}

// unitTargetWarning is the doctor/status warning for an installed user unit
// that will not start at boot, or "" when there is nothing to warn about.
func unitTargetWarning() string {
	if _, _, wrong := userUnitWrong(); !wrong {
		return ""
	}
	return "warning: " + userUnitName + " is WantedBy=multi-user.target, which the systemd user manager never reaches, so the daemon will not start at boot. " +
		"Fix: re-run `claude-memsync install` (or `claude-memsync start`), or by hand: " +
		"`systemctl --user disable " + userUnitName + "`, change WantedBy to default.target in the unit, `systemctl --user daemon-reload`, `systemctl --user enable " + userUnitName + "`"
}
