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
	keep := binLinkDirFn // unitEnv isolates it; the test's own bin dir wins
	path, _ := unitEnv(t, u)
	binLinkDirFn = keep
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
	exe := newBuild(t, lib)
	var err error
	captureStderr(t, func() { err = installTask("") })
	if err != nil {
		t.Fatal(err)
	}
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

// newBuild adds lib/new/claude-memsync (and a claude-memmerge beside it) under
// the lib root, makes it the installing executable and returns its path.
func newBuild(t *testing.T, lib map[string]string) string {
	t.Helper()
	root := filepath.Dir(filepath.Dir(lib["build-one"]))
	d := filepath.Join(root, "new")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"claude-memsync", "claude-memmerge"} {
		if err := os.WriteFile(filepath.Join(d, n), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	exe := filepath.Join(d, "claude-memsync")
	oldE, oldL := installExeFn, libRootFn
	installExeFn = func() (string, error) { return exe, nil }
	libRootFn = func() string { return root }
	t.Cleanup(func() { installExeFn, libRootFn = oldE, oldL })
	return exe
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
	exe := newBuild(t, lib)
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

func installQuietly(t *testing.T) (stderr string, err error) {
	t.Helper()
	kardianosInstall = func(svc service.Service) error { return nil }
	stderr = captureStderr(t, func() { err = installTask("") })
	return
}

func TestInstall_TempDirExeChangesNothing(t *testing.T) {
	lib, bin := buildDirs(t)
	path := unitFor(t, lib["build-one"], lib["build-one"])
	link := filepath.Join(bin, "claude-memsync")
	os.Symlink(lib["build-one"], link)
	exe := newBuild(t, lib)
	root := filepath.Dir(filepath.Dir(exe))
	oldR, oldT := tempRoots, exeThrowawayFn
	tempRoots = func() []string { return []string{root} }
	exeThrowawayFn = underTemp
	t.Cleanup(func() { tempRoots, exeThrowawayFn = oldR, oldT })
	before := readFile(t, path)
	stderr, err := installQuietly(t)
	if err != nil {
		t.Fatal(err)
	}
	if readFile(t, path) != before {
		t.Error("the unit must not change")
	}
	if got := real(t, link); got != real(t, lib["build-one"]) {
		t.Errorf("symlink moved to %s", got)
	}
	if !strings.Contains(stderr, "temp") {
		t.Errorf("want a warning about the temp dir: %q", stderr)
	}
}

func TestInstall_ScratchBuildKeepsLinksButRewritesUnit(t *testing.T) {
	lib, bin := buildDirs(t)
	path := unitFor(t, lib["build-one"], lib["build-one"])
	link := filepath.Join(bin, "claude-memsync")
	os.Symlink(lib["build-one"], link)
	exe := newBuild(t, lib)
	libRootFn = func() string { return filepath.Join(t.TempDir(), "elsewhere") } // exe is not under the lib root
	stderr, err := installQuietly(t)
	if err != nil {
		t.Fatal(err)
	}
	if got := real(t, link); got != real(t, lib["build-one"]) {
		t.Errorf("a scratch build must not repoint the link, got %s", got)
	}
	if got := real(t, condLine(t, path)); got != real(t, exe) {
		t.Errorf("Condition = %s, want %s", got, exe)
	}
	if !strings.Contains(stderr, "lib") {
		t.Errorf("want a warning that the links were left: %q", stderr)
	}
}

func TestInstall_RefusesAnUnsafeSibling(t *testing.T) {
	for _, name := range []string{"group-writable", "foreign-owner"} {
		t.Run(name, func(t *testing.T) {
			lib, bin := buildDirs(t)
			unitFor(t, lib["build-one"], lib["build-one"])
			exe := newBuild(t, lib)
			mm := filepath.Join(filepath.Dir(exe), "claude-memmerge")
			mlink := filepath.Join(bin, "claude-memmerge")
			os.Symlink(lib["build-one"], mlink)
			if name == "group-writable" {
				os.Chmod(mm, 0o775)
			} else {
				old := currentUID
				currentUID = func() int { return os.Getuid() + 1 }
				t.Cleanup(func() { currentUID = old })
			}
			stderr, _ := installQuietly(t)
			if got := real(t, mlink); got != real(t, lib["build-one"]) {
				t.Errorf("an unsafe sibling must not be linked, got %s", got)
			}
			if !strings.Contains(stderr, "claude-memmerge") {
				t.Errorf("want a warning naming it: %q", stderr)
			}
		})
	}
}

func TestUnitVersionCheck_NeverPrintsTheExecStartLine(t *testing.T) {
	lib, _ := buildDirs(t)
	path := unitFor(t, lib["build-one"], lib["build-one"])
	b := readFile(t, path)
	bad := strings.Replace(b, execLine(t, path), "ExecStart=env TOKEN=ghp_FAKESECRET123 /bin/x --key=ghp_FAKESECRET123 'unterminated", 1)
	os.WriteFile(path, []byte(bad), 0o644)
	r := unitVersionCheck()
	var out bytes.Buffer
	reportUnitVersion(&out, r)
	if r.OK {
		t.Fatal("an unreadable ExecStart must fail")
	}
	if strings.Contains(r.Detail+out.String(), "FAKESECRET") {
		t.Errorf("the ExecStart line leaked:\n%s", out.String())
	}
	if !strings.Contains(out.String(), path) {
		t.Errorf("want the unit path: %q", out.String())
	}
}

func throwawaySeams(t *testing.T, tmp, from string) {
	t.Helper()
	oT, oF := tempDirFn, ancestorsFrom
	tempDirFn = func() string { return tmp }
	ancestorsFrom = from
	t.Cleanup(func() { tempDirFn, ancestorsFrom = oT, oF })
}

func mkExe(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "claude-memsync")
	if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExeThrowaway_SymlinkIntoTempIsRefused(t *testing.T) {
	root := t.TempDir()
	tmp := filepath.Join(root, "tmp")
	exe := mkExe(t, tmp)
	other := filepath.Join(root, "lib", "id")
	os.MkdirAll(other, 0o755)
	via := filepath.Join(other, "claude-memsync")
	if err := os.Symlink(exe, via); err != nil {
		t.Fatal(err)
	}
	throwawaySeams(t, tmp, root)
	if !exeThrowaway(via) {
		t.Error("an exe reached through a symlink into the temp dir must be refused")
	}
}

func TestExeThrowaway_WorldWritableAncestorIsRefused(t *testing.T) {
	root := t.TempDir()
	d := filepath.Join(root, "shared")
	exe := mkExe(t, filepath.Join(d, "sub"))
	if err := os.Chmod(d, 0o777); err != nil {
		t.Fatal(err)
	}
	throwawaySeams(t, filepath.Join(root, "nottmp"), root)
	if !exeThrowaway(exe) {
		t.Error("an exe under a world-writable directory must be refused")
	}
}

func TestExeThrowaway_NormalLibPathIsAllowed(t *testing.T) {
	root := t.TempDir()
	exe := mkExe(t, filepath.Join(root, "home", ".local", "lib", "claude-memsync", "id"))
	throwawaySeams(t, filepath.Join(root, "nottmp"), root)
	if exeThrowaway(exe) {
		t.Error("a normal lib path must be allowed")
	}
}

func TestExeThrowaway_GroupWritableAncestorIsRefused(t *testing.T) {
	root := t.TempDir()
	d := filepath.Join(root, "grp")
	exe := mkExe(t, filepath.Join(d, "sub"))
	if err := os.Chmod(d, 0o775); err != nil {
		t.Fatal(err)
	}
	throwawaySeams(t, filepath.Join(root, "nottmp"), root)
	if !exeThrowaway(exe) {
		t.Error("an exe under a group-writable directory must be refused")
	}
}

func TestExeThrowaway_AncestorOfAnotherUserIsRefused(t *testing.T) {
	root := t.TempDir()
	exe := mkExe(t, filepath.Join(root, "home", "lib", "id"))
	throwawaySeams(t, filepath.Join(root, "nottmp"), root)
	old := currentUID
	currentUID = func() int { return os.Getuid() + 1 } // the directories now belong to someone else
	t.Cleanup(func() { currentUID = old })
	if !exeThrowaway(exe) {
		t.Error("a directory owned by another user (not root) must be refused")
	}
}

func TestExeThrowaway_GroupWritableExeIsRefused(t *testing.T) {
	root := t.TempDir()
	exe := mkExe(t, filepath.Join(root, "lib", "id"))
	if err := os.Chmod(exe, 0o775); err != nil {
		t.Fatal(err)
	}
	throwawaySeams(t, filepath.Join(root, "nottmp"), root)
	if !exeThrowaway(exe) {
		t.Error("a group-writable exe must be refused")
	}
}

func TestInstall_NeverPrintsExecStartArguments(t *testing.T) {
	lib, _ := buildDirs(t)
	path := unitFor(t, lib["build-one"], lib["build-one"])
	b := readFile(t, path)
	withTok := strings.Replace(b, execLine(t, path), execStartLine(lib["build-one"], []string{"run", "--token", "ghp_FAKESECRET123"}), 1)
	os.WriteFile(path, []byte(withTok), 0o644)
	newBuild(t, lib)
	stderr, err := installQuietly(t)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr, "FAKESECRET") || strings.Contains(stderr, "--token") {
		t.Errorf("install printed argument text:\n%s", stderr)
	}
	if !strings.Contains(stderr, "arguments unchanged") {
		t.Errorf("want 'arguments unchanged': %q", stderr)
	}
}
