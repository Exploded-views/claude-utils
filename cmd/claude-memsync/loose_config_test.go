//go:build !windows

package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/testenv"
)

func looseConfig(t *testing.T, cfg config.Config) (before []byte) {
	t.Helper()
	if err := os.MkdirAll(cfg.SyncDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfg.Path(), 0o644); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(cfg.Path())
	return b
}

func assertUntouched(t *testing.T, path string, before []byte) {
	t.Helper()
	after, _ := os.ReadFile(path)
	st, _ := os.Stat(path)
	if !bytes.Equal(after, before) || st.Mode().Perm() != 0o644 {
		t.Errorf("the config was rewritten or its mode changed: mode %04o\n%s", st.Mode().Perm(), after)
	}
}

// A config that cannot be loaded is never rewritten with defaults: re-init of a
// read-only host with a 0644 config refuses, names the fix and leaves the file.
func TestReinit_RefusesALooseConfigAndLeavesItAlone(t *testing.T) {
	cfg, _ := initFixture(t)
	old := cfg
	old.ReadOnly = true
	old.HostLabel = "vm-ro"
	before := looseConfig(t, old)
	err := bootstrap(cfg, true, nil)
	if err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("want a refusal naming chmod 600, got %v", err)
	}
	assertUntouched(t, cfg.Path(), before)
}

// unlabelledEncRepo is an encrypted sync repo whose own key has no host label.
func unlabelledEncRepo(t *testing.T) config.Config {
	t.Helper()
	cfg, _ := initFixture(t)
	t.Setenv("HOME", t.TempDir())
	ms, mm := testenv.Binaries(t)
	cfg.MergeDriverPath = mm
	enc := &encryptSpec{IdentityPath: filepath.Join(t.TempDir(), "k", "identity.txt"), MemsyncExe: ms}
	if err := bootstrapWith(cfg, false, nil, enc); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestRecipientsLabel_RefusesALooseConfig(t *testing.T) {
	cfg := unlabelledEncRepo(t)
	before := looseConfig(t, cfg)
	var out, errb bytes.Buffer
	locker := func(_ io.Writer, _ string, fn func() error) error { return fn() }
	if code := labelSelfOne(&out, &errb, cfg, "vm-new", locker); code == 0 || !strings.Contains(errb.String(), "chmod 600") {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
	assertUntouched(t, cfg.Path(), before)
}

func TestRecipientsRecovery_RefusesALooseConfig(t *testing.T) {
	cfg := unlabelledEncRepo(t)
	_, rc := newTestIdentity(t, "recovery")
	if err := agecrypt.AddRecipientLabelled(gitwt.New(cfg.SyncDir), rc, ""); err != nil {
		t.Fatal(err)
	}
	before := looseConfig(t, cfg)
	var out, errb bytes.Buffer
	locker := func(_ io.Writer, _ string, fn func() error) error { return fn() }
	if code := labelRecoveryOne(&out, &errb, cfg, rc, locker); code == 0 || !strings.Contains(errb.String(), "chmod 600") {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
	assertUntouched(t, cfg.Path(), before)
}

// doctor shows the refusal plainly, with the fix, instead of checking defaults.
func TestDoctor_ShowsALooseConfigPlainly(t *testing.T) {
	cfg, _ := initFixture(t)
	looseConfig(t, cfg)
	var out, errb bytes.Buffer
	code := doctorCmd(&out, &errb, []string{"--config", cfg.Path()})
	all := out.String() + errb.String()
	if code != 2 || !strings.Contains(all, "chmod 600 "+cfg.Path()) {
		t.Fatalf("code %d, output %q", code, all)
	}
}

func TestInitGuard_RefusesALooseConfig(t *testing.T) {
	cfg, _ := initFixture(t)
	t.Setenv(routesEnv, "")
	looseConfig(t, cfg)
	if err := initGuard(cfg, nil, guardOpts{SkipQuiet: true}); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("the foreign-writer guard must not lose its list silently, got %v", err)
	}
}
