package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/config"
)

func TestInitExpires_RejectsBadAndPastDatesBeforeTouchingAnything(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sync")
	for _, d := range []string{"soon", "2026-13-40", "2001-01-01", "06/01/2030"} {
		code := runInit([]string{"--remote", "/nonexistent/origin.git", "--sync-dir", dir, "--expires", d})
		if code != 2 {
			t.Errorf("--expires %q: want a usage error, got %d", d, code)
		}
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("a refused init must not create the sync dir")
	}
}

func TestExpiryLine(t *testing.T) {
	now := time.Date(2026, 6, 10, 9, 0, 0, 0, time.UTC)
	var c config.Config
	if expiryLine(c, now) != "" {
		t.Error("no expiry, no line")
	}
	c.ExpiresAt = "2026-07-01"
	if l := expiryLine(c, now); !strings.Contains(l, "2026-07-01") || strings.Contains(l, "retire me") {
		t.Errorf("upcoming: %q", l)
	}
	c.ExpiresAt = "2026-06-09"
	if l := expiryLine(c, now); !strings.Contains(l, "expired: retire me") {
		t.Errorf("expired: %q", l)
	}
}

func TestPublishExpiry(t *testing.T) {
	cfg := config.Defaults()
	cfg.HostLabel, cfg.ExpiresAt = "vm-a", "2026-09-30"
	reg := newRegistryRepoForCmd(t)
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, f) }
	if err := publishExpiry(cfg, reg, false, logf); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(reg, "hosts", "vm-a", "expiry"))
	if err != nil || string(b) != "2026-09-30\n" {
		t.Fatalf("%q %v", b, err)
	}
	if err := publishExpiry(cfg, reg, false, logf); err != nil { // idempotent
		t.Fatal(err)
	}
	// no label: nothing to publish under, and it says so
	nl := cfg
	nl.HostLabel = ""
	if err := publishExpiry(nl, reg, false, logf); err == nil {
		t.Error("an expiry without a host label cannot be published")
	}
	// a read-only registry is never committed into
	reg2 := newRegistryRepoForCmd(t)
	if err := publishExpiry(cfg, reg2, true, logf); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(reg2, "hosts")); err == nil {
		t.Error("a read-only registry must not be written")
	}
	// no expiry configured: nothing
	cfg.ExpiresAt = ""
	reg3 := newRegistryRepoForCmd(t)
	if err := publishExpiry(cfg, reg3, false, logf); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(reg3, "hosts")); err == nil {
		t.Error("no expiry, nothing published")
	}
}

func newRegistryRepoForCmd(t *testing.T) string {
	t.Helper()
	initEnv(t)
	dir := t.TempDir()
	if out, err := execGit(dir, "init", "-q", "-b", "main"); err != nil {
		t.Fatal(out, err)
	}
	return dir
}
