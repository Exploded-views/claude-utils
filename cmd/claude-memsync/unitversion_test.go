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

// buildDirs makes lib/<id>/claude-memsync executables for three old builds and
// returns their paths plus a bin dir wired into binLinkDirFn.
func buildDirs(t *testing.T) (lib map[string]string, bin string) {
	t.Helper()
	root := t.TempDir()
	lib = map[string]string{}
	for _, id := range []string{"build-one", "build-two", "build-three"} {
		d := filepath.Join(root, "lib", id)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(d, "claude-memsync")
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		lib[id] = p
	}
	bin = filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	old := binLinkDirFn
	binLinkDirFn = func() (string, error) { return bin, nil }
	t.Cleanup(func() { binLinkDirFn = old })
	return lib, bin
}

// unitFor is a confirmed unit whose Condition and ExecStart name the given builds.
func unitFor(t *testing.T, cond, exec string) string {
	t.Helper()
	u := strings.Replace(brokenUnit, "multi-user.target", "default.target", 1)
	u = strings.Replace(u, "ConditionFileIsExecutable=/usr/local/bin/claude-memsync", "ConditionFileIsExecutable="+cond, 1)
	u = strings.Replace(u, `ExecStart=/usr/local/bin/claude-memsync "run"`, execStartLine(exec, []string{"run"}), 1)
	path, _ := unitEnv(t, u)
	if err := os.MkdirAll(filepath.Dir(wantsLink(path)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, wantsLink(path)); err != nil {
		t.Fatal(err)
	}
	return path
}

func condLine(t *testing.T, path string) string {
	t.Helper()
	b, _ := os.ReadFile(path)
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "ConditionFileIsExecutable=") {
			return strings.TrimPrefix(l, "ConditionFileIsExecutable=")
		}
	}
	return ""
}

func real(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestInstall_RewritesConditionExecStartAndSymlinkTogether(t *testing.T) {
	lib, bin := buildDirs(t)
	path := unitFor(t, lib["build-one"], lib["build-two"])
	link := filepath.Join(bin, "claude-memsync")
	if err := os.Symlink(lib["build-three"], link); err != nil {
		t.Fatal(err)
	}
	kardianosInstall = func(svc service.Service) error { return nil }
	var err error
	captureStderr(t, func() { err = installTask("") })
	if err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	want := real(t, exe)
	if got := real(t, condLine(t, path)); got != want {
		t.Errorf("Condition = %s, want the new build %s", got, want)
	}
	cur, _ := currentExecStart(readFile(t, path))
	if e, _, _ := parseExecStart(cur); real(t, e) != want {
		t.Errorf("ExecStart = %s, want the new build %s", e, want)
	}
	if got := real(t, link); got != want {
		t.Errorf("bin symlink -> %s, want the new build %s", got, want)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// An upgrade stopped right after the symlink step: the link is on the new
// build, the unit still names the old one. The next install repairs the unit.
func TestInstall_RepairsAnUpgradeInterruptedAfterTheSymlinkStep(t *testing.T) {
	lib, bin := buildDirs(t)
	exe, _ := os.Executable()
	path := unitFor(t, lib["build-one"], lib["build-one"])
	link := filepath.Join(bin, "claude-memsync")
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	kardianosInstall = func(svc service.Service) error { return nil }
	var err error
	captureStderr(t, func() { err = installTask("") })
	if err != nil {
		t.Fatal(err)
	}
	if r := unitVersionCheck(); !r.OK {
		t.Errorf("after the re-run the pieces must agree: %s", r.Detail)
	}
	if got := real(t, condLine(t, path)); got != real(t, exe) {
		t.Errorf("Condition = %s, want %s", got, exe)
	}
}

func TestUnitVersionCheck(t *testing.T) {
	lib, bin := buildDirs(t)
	one, two, three := lib["build-one"], lib["build-two"], lib["build-three"]
	link := filepath.Join(bin, "claude-memsync")
	cases := []struct {
		name         string
		cond, exec   string
		link         string
		ok           bool
		mention, not []string
	}{
		{"all agree", one, one, one, true, nil, nil},
		{"condition only", two, one, one, false, []string{"Condition", two, "ExecStart", "symlink", one}, nil},
		{"ExecStart only", one, two, one, false, []string{"ExecStart", two, "Condition", "symlink", one}, nil},
		{"symlink only", one, one, three, false, []string{"symlink", three, "Condition", "ExecStart", one}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			unitFor(t, c.cond, c.exec)
			os.Remove(link)
			if err := os.Symlink(c.link, link); err != nil {
				t.Fatal(err)
			}
			r := unitVersionCheck()
			if r.ID != "unit-version" {
				t.Errorf("id = %q", r.ID)
			}
			if r.OK != c.ok {
				t.Fatalf("OK = %v, want %v (%s)", r.OK, c.ok, r.Detail)
			}
			for _, m := range c.mention {
				if !c.ok && !strings.Contains(r.Detail, m) {
					t.Errorf("detail lacks %q:\n%s", m, r.Detail)
				}
			}
			var out, errw bytes.Buffer
			if got := reportUnitVersion(&errw, r); got != c.ok {
				t.Errorf("report = %v, want %v", got, c.ok)
			}
			_ = out
			if !c.ok && !strings.Contains(errw.String(), "unit-version") {
				t.Errorf("the report must carry the id: %q", errw.String())
			}
		})
	}
}

func TestUnitVersionCheck_NoUnitIsNotAFailure(t *testing.T) {
	buildDirs(t)
	unitEnv(t, "")
	if r := unitVersionCheck(); !r.OK {
		t.Errorf("no unit: %s", r.Detail)
	}
}
