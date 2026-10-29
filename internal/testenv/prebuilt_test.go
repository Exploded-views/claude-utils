package testenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const prebuiltTestTag = "v9.9.9-prebuilt.test"

// fatalTB records Fatal and Skip instead of ending the test, so a test can
// assert that the harness fails (and does not skip) a bad prebuilt set-up.
type fatalTB struct {
	testing.TB
	fatal, skipped bool
	msg            string
}

func (f *fatalTB) Helper() {}
func (f *fatalTB) Fatalf(format string, a ...any) {
	f.fatal, f.msg = true, format
	runtime.Goexit()
}
func (f *fatalTB) Fatal(a ...any)           { f.fatal = true; runtime.Goexit() }
func (f *fatalTB) SkipNow()                 { f.skipped = true; runtime.Goexit() }
func (f *fatalTB) Skip(a ...any)            { f.SkipNow() }
func (f *fatalTB) Skipf(s string, a ...any) { f.SkipNow() }

// runRecorded runs fn in its own goroutine, as t.Fatal would end it.
func runRecorded(fn func(tb testing.TB)) *fatalTB {
	f := &fatalTB{}
	done := make(chan struct{})
	go func() { defer close(done); fn(f) }()
	<-done
	return f
}

func envMap(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// A fake go first on PATH, recording any call. The caller builds its binaries
// before installing it.
func installFakeGo(t *testing.T) (marker string) {
	t.Helper()
	d := t.TempDir()
	marker = filepath.Join(d, "go-was-called")
	script := "#!/bin/sh\necho \"$@\" >> '" + marker + "'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(d, "go"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", d+string(os.PathListSeparator)+os.Getenv("PATH"))
	return marker
}

func TestBudgetPrebuiltModeNeverRunsGo(t *testing.T) {
	ms0, _ := BinariesVersion(t, prebuiltTestTag) // out of band, with the real go
	dir := filepath.Dir(ms0)
	marker := installFakeGo(t)
	t.Setenv(EnvBinDir, dir)
	t.Setenv(EnvTag, prebuiltTestTag)

	ms, mm := Binaries(t)
	if ms != filepath.Join(dir, "claude-memsync") || mm != filepath.Join(dir, "claude-memmerge") {
		t.Fatalf("Binaries = %s, %s; want the prebuilt ones in %s", ms, mm, dir)
	}
	out, err := exec.Command(ms, "version").Output() // a cheap gate path
	if err != nil || strings.TrimSpace(string(out)) != "claude-memsync "+prebuiltTestTag {
		t.Fatalf("version = %q, %v", out, err)
	}
	// The set-up that runs before the gates builds nothing either.
	orig := filepath.Join(t.TempDir(), "orig-home")
	cleanup, err := prebuiltSetup(os.Getenv, orig, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if _, err := os.Stat(marker); err == nil {
		b, _ := os.ReadFile(marker)
		t.Fatalf("go was called in prebuilt mode: %s", b)
	}
}

func TestBudgetPrebuiltVersionMismatchFails(t *testing.T) {
	ms0, _ := BinariesVersion(t, prebuiltTestTag)
	dir := filepath.Dir(ms0)
	t.Setenv(EnvBinDir, dir)
	t.Setenv(EnvTag, "v1.2.3-other")

	f := runRecorded(func(tb testing.TB) { Binaries(tb) })
	if !f.fatal || f.skipped {
		t.Fatalf("a binary stamped %s under tag v1.2.3-other: fatal=%v skipped=%v, want a failure", prebuiltTestTag, f.fatal, f.skipped)
	}
	if _, err := prebuiltSetup(os.Getenv, filepath.Join(t.TempDir(), "orig"), t.TempDir()); err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("prebuiltSetup with a mismatched tag: %v, want a version error before any gate", err)
	}
}

func TestBudgetPrebuiltNeedsBothVars(t *testing.T) {
	ms0, _ := BinariesVersion(t, prebuiltTestTag) // real binaries: only the missing variable can be the reason
	for name, env := range map[string]map[string]string{
		"dir only": {EnvBinDir: filepath.Dir(ms0)},
		"tag only": {EnvTag: prebuiltTestTag},
	} {
		if _, _, _, err := prebuiltBinaries(envMap(env)); err == nil || !strings.Contains(err.Error(), "together") {
			t.Errorf("%s: err = %v, want the set-both-or-neither error", name, err)
		}
		if _, err := prebuiltSetup(envMap(env), filepath.Join(t.TempDir(), "o"), t.TempDir()); err == nil {
			t.Errorf("%s: set-up did not fail", name)
		}
	}
	t.Setenv(EnvBinDir, filepath.Dir(ms0))
	t.Setenv(EnvTag, "")
	os.Unsetenv(EnvTag)
	if f := runRecorded(func(tb testing.TB) { Binaries(tb) }); !f.fatal || f.skipped {
		t.Errorf("Binaries with the dir alone: fatal=%v skipped=%v, want a failure", f.fatal, f.skipped)
	}
	if _, _, on, err := prebuiltBinaries(envMap(nil)); on || err != nil {
		t.Errorf("neither var: on=%v err=%v, want source mode", on, err)
	}
}

func TestBudgetPrebuiltRefusesRealHome(t *testing.T) {
	ms0, _ := BinariesVersion(t, prebuiltTestTag)
	env := map[string]string{EnvBinDir: filepath.Dir(ms0), EnvTag: prebuiltTestTag}
	for _, k := range []string{"HOME", "XDG_CONFIG_HOME", "TMPDIR"} {
		t.Setenv(k, os.Getenv(k)) // restored at the end, whatever the set-up does
	}
	before := os.Getenv("HOME")

	// A stand-in for the original HOME, made here; the would-be temp HOME lands under it.
	orig := t.TempDir()
	base := filepath.Join(orig, "tmp")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := prebuiltSetup(envMap(env), orig, base); err == nil || !strings.Contains(err.Error(), "original HOME") {
		t.Fatalf("set-up under the original HOME: %v, want a refusal", err)
	}
	if os.Getenv("HOME") != before {
		t.Errorf("HOME changed to %s although the set-up refused", os.Getenv("HOME"))
	}
	if ents, _ := os.ReadDir(base); len(ents) != 0 {
		t.Errorf("the refused set-up left %d entries under the original HOME", len(ents))
	}

	// A routes path the harness computes that escapes the temp HOME is refused too.
	_, _, err := isolateHome(orig, t.TempDir(), func(home string) []string {
		return []string{filepath.Join(orig, ".config", "claude-memsync", "routes.json")}
	})
	if err == nil || !strings.Contains(err.Error(), "original HOME") {
		t.Fatalf("a computed path under the original HOME: %v, want a refusal", err)
	}
	_, _, err = isolateHome(orig, t.TempDir(), func(home string) []string { return []string{filepath.Join(t.TempDir(), "elsewhere")} })
	if err == nil {
		t.Fatal("a computed path outside the temp HOME was accepted")
	}
}

func TestBudgetPrebuiltIsolatesHome(t *testing.T) {
	ms0, _ := BinariesVersion(t, prebuiltTestTag)
	env := map[string]string{EnvBinDir: filepath.Dir(ms0), EnvTag: prebuiltTestTag}
	keys := []string{"HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "TMPDIR"}
	for _, k := range keys {
		t.Setenv(k, os.Getenv(k))
	}
	orig := filepath.Join(t.TempDir(), "orig-home")
	cleanup, err := prebuiltSetup(envMap(env), orig, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := os.Getenv("HOME")
	if home == "" || home == orig {
		t.Fatalf("HOME = %q", home)
	}
	for _, k := range keys {
		if v := os.Getenv(k); v != home && !strings.HasPrefix(v, home+string(os.PathSeparator)) {
			t.Errorf("%s = %s, not under the temp HOME %s", k, v, home)
		}
	}
	cleanup()
	if _, err := os.Stat(home); err == nil {
		t.Errorf("cleanup left %s", home)
	}
}
