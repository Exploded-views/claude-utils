package config

import (
	"os"
	"path/filepath"
	"testing"
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
