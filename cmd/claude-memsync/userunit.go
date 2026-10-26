package main

import (
	"errors"
	"fmt"
	"io"
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
	userTarget    = "default.target"
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

// installLines are the indexes of the lines inside the [Install] section.
func installLines(lines []string) []int {
	var idx []int
	in := false
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			in = t == "[Install]"
			continue
		}
		if in {
			idx = append(idx, i)
		}
	}
	return idx
}

// fixUnitTarget points the [Install] WantedBy of a user unit at
// default.target. Only a whole line `WantedBy=multi-user.target` inside
// [Install] changes; comments, other sections and multi-value lines are left
// as they are. changed is false when there is nothing to do, so the fix is
// idempotent.
func fixUnitTarget(content string) (string, bool) {
	lines := strings.Split(content, "\n")
	changed := false
	for _, i := range installLines(lines) {
		if strings.TrimSuffix(lines[i], "\r") == wrongWantedBy {
			lines[i] = strings.Replace(lines[i], wrongWantedBy, rightWantedBy, 1)
			changed = true
		}
	}
	return strings.Join(lines, "\n"), changed
}

// targetRight is true when [Install] has exactly one WantedBy line and it is
// `WantedBy=default.target`.
func targetRight(content string) bool {
	lines := strings.Split(content, "\n")
	n := 0
	for _, i := range installLines(lines) {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "WantedBy=") {
			n++
			if strings.TrimSuffix(lines[i], "\r") != rightWantedBy {
				return false
			}
		}
	}
	return n == 1
}

// linkRight is true when default.target.wants/<unit> is a symlink (lstat, not
// followed for the check) that resolves to the unit. Any error is "no".
func linkRight(unitPath string) bool {
	link := filepath.Join(filepath.Dir(unitPath), userTarget+".wants", userUnitName)
	st, err := os.Lstat(link)
	if err != nil || st.Mode()&os.ModeSymlink == 0 {
		return false
	}
	got, err := filepath.EvalSymlinks(link)
	if err != nil {
		return false
	}
	want, err := filepath.EvalSymlinks(unitPath)
	return err == nil && got == want
}

// autostartConfirmed is the only way a unit counts as OK: the file says
// default.target AND the enable link exists. A rewritten file whose reload or
// enable failed is therefore still reported.
func autostartConfirmed(path, content string) bool {
	return targetRight(content) && linkRight(path)
}

// readUserUnit reads the installed unit. ok is false on a non-Linux host, a
// missing or an unreadable unit.
func readUserUnit() (path, content string, ok bool) {
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
	return p, string(b), true
}

// userUnitWrong reports an installed unit that is not confirmed to autostart.
func userUnitWrong() (path, content string, wrong bool) {
	p, c, ok := readUserUnit()
	if !ok {
		return "", "", false
	}
	return p, c, !autostartConfirmed(p, c)
}

// execStartLine is the ExecStart line kardianos writes for exe and args.
func execStartLine(exe string, args []string) string {
	l := "ExecStart=" + strings.ReplaceAll(exe, " ", `\x20`)
	for _, a := range args {
		l += ` "` + strings.ReplaceAll(a, `"`, `\"`) + `"`
	}
	return l
}

// setExecStart replaces the unit's ExecStart line with want.
func setExecStart(content, want string) (string, bool) {
	lines := strings.Split(content, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "ExecStart=") {
			if l == want {
				return content, false
			}
			lines[i] = want
			return strings.Join(lines, "\n"), true
		}
	}
	return content, false
}

func manualSteps() string {
	return "to fix it by hand: `systemctl --user disable " + userUnitName + "`, change the WantedBy line in [Install] to `WantedBy=" + userTarget + "`, " +
		"`systemctl --user daemon-reload`, `systemctl --user enable " + userUnitName + "` (needs a running user manager: log in, or `loginctl enable-linger`)"
}

// ensureUserUnitTarget repairs the autostart of an installed user unit.
func ensureUserUnitTarget(w io.Writer) error { return repairUserUnit(w, "") }

// repairUserUnit makes an installed user unit start at boot, and, when
// wantExec is not empty, gives it that ExecStart. A unit that is confirmed
// (see autostartConfirmed) with the right ExecStart, an absent unit, or a
// non-Linux host is left alone and nothing is printed.
//
// The user manager is probed first (daemon-reload): if it is unreachable
// nothing is changed and the manual steps are printed. Otherwise: disable
// (while the old file still names multi-user.target, so its .wants link goes),
// rewrite atomically, reload, enable, and confirm.
func repairUserUnit(w io.Writer, wantExec string) error {
	path, content, ok := readUserUnit()
	if !ok {
		return nil
	}
	fixed, targetChanged := fixUnitTarget(content)
	execChanged := false
	if wantExec != "" {
		fixed, execChanged = setExecStart(fixed, wantExec)
	}
	if !execChanged && autostartConfirmed(path, content) {
		return nil
	}
	if !targetRight(fixed) {
		fmt.Fprintln(w, userUnitName+": the [Install] WantedBy is not a plain line this program can rewrite safely; "+manualSteps())
		return fmt.Errorf("%s: cannot repair the WantedBy line automatically", path)
	}
	if err := systemctlFn("daemon-reload"); err != nil {
		fmt.Fprintln(w, userUnitName+": the systemd user manager is not reachable, so nothing was changed; "+manualSteps())
		return fmt.Errorf("systemd user manager unreachable: %w", err)
	}
	if targetChanged {
		_ = systemctlFn("disable", userUnitName) // best effort: the unit may not be enabled
	}
	if fixed != content {
		if err := writeFileAtomic(path, []byte(fixed)); err != nil {
			fmt.Fprintln(w, userUnitName+": "+err.Error()+"; "+manualSteps())
			return fmt.Errorf("fix %s: %w", path, err)
		}
	}
	if err := systemctlFn("daemon-reload"); err != nil {
		return err
	}
	if err := systemctlFn("enable", userUnitName); err != nil {
		return err
	}
	if b, err := os.ReadFile(path); err != nil || !autostartConfirmed(path, string(b)) {
		return fmt.Errorf("%s: enabled, but %s.wants/%s is not a link to the unit", path, userTarget, userUnitName)
	}
	if targetChanged {
		fmt.Fprintf(w, "fixed autostart: %s is now WantedBy=default.target (was multi-user.target, which a user manager never reaches); reloaded and re-enabled\n", userUnitName)
	} else if !execChanged {
		fmt.Fprintf(w, "re-enabled %s: it said default.target but was not linked into %s.wants\n", userUnitName, userTarget)
	}
	if execChanged {
		fmt.Fprintf(w, "updated the unit's ExecStart to the requested arguments; restart the daemon to apply them (claude-memsync stop; claude-memsync start)\n")
	}
	return nil
}

// writeFileAtomic replaces path with data, keeping its mode: temp file in the
// same directory, fsync, rename, fsync of the directory. It refuses a symlink.
func writeFileAtomic(path string, data []byte) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; not replacing it, fix the link target by hand", path)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".claude-memsync-unit-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Chmod(st.Mode().Perm())
	}
	if err == nil {
		err = tmp.Sync()
	}
	err = errors.Join(err, tmp.Close())
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		os.Remove(name)
		return err
	}
	if d, derr := os.Open(dir); derr == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// unitTargetWarning is the doctor/status warning for an installed user unit
// that is not confirmed to start at boot, or "" when there is nothing to warn
// about.
func unitTargetWarning() string {
	if _, _, wrong := userUnitWrong(); !wrong {
		return ""
	}
	return "warning: " + userUnitName + " is not set up to start at boot: it needs WantedBy=default.target and a link in default.target.wants " +
		"(older versions wrote WantedBy=multi-user.target, which the systemd user manager never reaches, so the daemon did not start after a reboot). " +
		"Fix: re-run `claude-memsync install` (or `claude-memsync start`); " + manualSteps()
}
