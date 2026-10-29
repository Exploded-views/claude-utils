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

// asTestBinaryVersion makes the running test binary report v, as a release-stamped one would.
func asTestBinaryVersion(t *testing.T, v string) {
	t.Helper()
	old := selfVersion
	selfVersion = func() string { return v }
	t.Cleanup(func() { selfVersion = old })
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
	asTestBinaryVersion(t, prebuiltTestTag)
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
	asTestBinaryVersion(t, "v1.2.3-other")
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
	asTestBinaryVersion(t, prebuiltTestTag)
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
	asTestBinaryVersion(t, prebuiltTestTag)
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

// B1, B4, B7 and B8 run the test binary's own code, so its version must be the
// tag too: an old budget archive unpacked beside a new product archive must fail.
func TestBudgetPrebuiltTestBinaryVersionMustMatch(t *testing.T) {
	ms0, _ := BinariesVersion(t, prebuiltTestTag) // the product binaries match the tag
	env := envMap(map[string]string{EnvBinDir: filepath.Dir(ms0), EnvTag: prebuiltTestTag})
	for _, k := range []string{"HOME", "XDG_CONFIG_HOME", "TMPDIR"} {
		t.Setenv(k, os.Getenv(k))
	}
	asTestBinaryVersion(t, "v0.0.1-old-budget-archive")
	home := os.Getenv("HOME")
	cleanup, err := prebuiltSetup(env, filepath.Join(t.TempDir(), "orig"), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "test binary") {
		t.Fatalf("a test binary stamped with another tag: err = %v, want a refusal naming the test binary", err)
	}
	if cleanup != nil {
		t.Error("a cleanup came back with the error")
	}
	if os.Getenv("HOME") != home {
		t.Error("HOME changed although the set-up refused")
	}
	asTestBinaryVersion(t, prebuiltTestTag)
	cleanup, err = prebuiltSetup(env, filepath.Join(t.TempDir(), "orig"), t.TempDir())
	if err != nil {
		t.Fatalf("a matching test binary: %v", err)
	}
	cleanup()
}

func TestBudgetPrebuiltScrubsGitAndClaudeEnv(t *testing.T) {
	asTestBinaryVersion(t, prebuiltTestTag)
	ms0, _ := BinariesVersion(t, prebuiltTestTag)
	env := envMap(map[string]string{EnvBinDir: filepath.Dir(ms0), EnvTag: prebuiltTestTag})
	for _, k := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "TMPDIR", "USERPROFILE"} {
		t.Setenv(k, os.Getenv(k))
	}
	stand := t.TempDir()
	for k, v := range map[string]string{
		"GIT_DIR": stand, "GIT_WORK_TREE": stand, "GIT_CONFIG_GLOBAL": filepath.Join(stand, "gitconfig"),
		"GIT_CONFIG_PARAMETERS": "'a.b=c'", "GIT_SSH_COMMAND": "ssh -i " + stand + "/key", "GIT_AUTHOR_NAME": "inherited",
		"CLAUDE_PROJECT_DIR": stand, "SSH_AUTH_SOCK": stand + "/agent", "GIT_CONFIG_NOSYSTEM": "",
	} {
		t.Setenv(k, v)
	}
	cleanup, err := prebuiltSetup(env, filepath.Join(t.TempDir(), "orig"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("env").Output() // what a binary under test starts with
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, l := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			got[k] = v
		}
	}
	home := got["HOME"]
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_CONFIG_PARAMETERS", "GIT_AUTHOR_NAME", "CLAUDE_PROJECT_DIR", "SSH_AUTH_SOCK"} {
		if v, ok := got[k]; ok {
			t.Errorf("%s=%s is inherited by the binaries under test", k, v)
		}
	}
	if g := got["GIT_CONFIG_GLOBAL"]; g != filepath.Join(home, ".gitconfig") {
		t.Errorf("GIT_CONFIG_GLOBAL = %q, want a file under the temp HOME %s", g, home)
	}
	if got["GIT_CONFIG_NOSYSTEM"] != "1" {
		t.Errorf("GIT_CONFIG_NOSYSTEM = %q, want 1", got["GIT_CONFIG_NOSYSTEM"])
	}
	if c := got["GIT_SSH_COMMAND"]; c == "" || strings.Contains(c, stand) {
		t.Errorf("GIT_SSH_COMMAND = %q, want a command that fails", c)
	} else if err := exec.Command("sh", "-c", c).Run(); err == nil {
		t.Errorf("GIT_SSH_COMMAND %q succeeds; ssh must never be reachable", c)
	}
	cleanup()
	if os.Getenv("GIT_DIR") != stand || os.Getenv("CLAUDE_PROJECT_DIR") != stand {
		t.Error("cleanup did not restore the inherited variables")
	}
}

func TestBudgetPrebuiltRefusesMissingHome(t *testing.T) {
	asTestBinaryVersion(t, prebuiltTestTag)
	ms0, _ := BinariesVersion(t, prebuiltTestTag)
	env := envMap(map[string]string{EnvBinDir: filepath.Dir(ms0), EnvTag: prebuiltTestTag})
	base := t.TempDir()
	if _, err := prebuiltSetup(env, "", base); err == nil || !strings.Contains(err.Error(), "HOME") {
		t.Fatalf("no HOME at start: err = %v, want a refusal naming HOME", err)
	}
	if ents, _ := os.ReadDir(base); len(ents) != 0 {
		t.Errorf("the refused set-up left %d entries", len(ents))
	}
	// Source mode needs no HOME check: nothing is isolated there.
	if _, err := prebuiltSetup(envMap(nil), "", base); err != nil {
		t.Errorf("source mode with no HOME: %v", err)
	}
}

func TestBudgetPrebuiltTagMustBeAReleaseTag(t *testing.T) {
	ms0, _ := BinariesVersion(t, prebuiltTestTag)
	for _, tag := range []string{"dev", "1.2.3", "v1", "v1.2", "v1.2.3+meta", "v1.2.3-", "vfoo"} {
		env := envMap(map[string]string{EnvBinDir: filepath.Dir(ms0), EnvTag: tag})
		if _, _, _, err := prebuiltBinaries(env); err == nil || !strings.Contains(err.Error(), "release tag") {
			t.Errorf("tag %q: err = %v, want a release-tag refusal", tag, err)
		}
	}
	for _, tag := range []string{"v0.3.0", "v0.3.0-ev.1", "v9.9.9-prebuilt.test"} {
		if !releaseTagRE.MatchString(tag) {
			t.Errorf("%q should be a release tag", tag)
		}
	}
}
