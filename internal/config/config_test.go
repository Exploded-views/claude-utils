package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad_ScrubDefaultsOnForExistingConfigs(t *testing.T) {
	// A config written before the scrub options existed.
	c, err := Load(writeCfg(t, `{"remoteUrl":"git@example.com:me/mem.git","debounceMs":1000}`))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Scrub {
		t.Error("scrub should default to true")
	}
	if c.RequireDenylist {
		t.Error("requireDenylist should default to false")
	}
}

func TestLoad_ScrubOptions(t *testing.T) {
	c, err := Load(writeCfg(t, `{"scrub":false,"requireDenylist":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Scrub || !c.RequireDenylist {
		t.Errorf("got scrub=%v requireDenylist=%v", c.Scrub, c.RequireDenylist)
	}
}

func TestDenylistPath_SitsNextToConfig(t *testing.T) {
	c := Defaults()
	c.SyncDir = filepath.Join("home", "alice", ".claudesync")
	if filepath.Dir(c.DenylistPath()) != filepath.Dir(c.Path()) {
		t.Errorf("denylist %s not next to config %s", c.DenylistPath(), c.Path())
	}
}

func TestExpiry(t *testing.T) {
	c, err := Load(writeCfg(t, `{"expiresAt":"2026-06-01"}`))
	if err != nil {
		t.Fatal(err)
	}
	day := func(s string) time.Time { x, _ := time.Parse(time.RFC3339, s); return x }
	if c.Expired(day("2026-05-31T23:59:59Z")) || c.Expired(day("2026-06-01T00:00:00Z")) || c.Expired(day("2026-06-01T23:59:59Z")) {
		t.Error("the host is usable through its last day")
	}
	if !c.Expired(day("2026-06-02T00:00:00Z")) {
		t.Error("the day after the date it is expired")
	}
	var none Config
	if none.Expired(day("2100-01-01T00:00:00Z")) {
		t.Error("no expiry never expires")
	}
	if _, ok := none.ExpiryDate(); ok {
		t.Error("no date set")
	}
	if _, err := Load(writeCfg(t, `{"expiresAt":"next week"}`)); err == nil {
		t.Error("a malformed expiry must fail loudly, not silently never expire")
	}
}
