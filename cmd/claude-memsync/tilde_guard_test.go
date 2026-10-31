package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/config"
)

// A host stores "~/" paths in routes.json. Every guard reads the config by the
// same expansion the daemon uses (route.ExpandHome), and a path left literal
// is an error, never a quiet skip.
func tildeHost(t *testing.T) (cfgs map[string]config.Config, routes string) {
	t.Helper()
	cfgs = routedHost(t, []string{"personal"}, "personal", nil)
	routes = os.Getenv(routesEnv)
	var rj map[string]any
	b, _ := os.ReadFile(routes)
	if err := json.Unmarshal(b, &rj); err != nil {
		t.Fatal(err)
	}
	rj["repos"].(map[string]any)["personal"].(map[string]any)["config"] = "~/.claudesync.d/personal/config.json"
	b, _ = json.Marshal(rj)
	if err := os.WriteFile(routes, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgs, routes
}

func TestTildePaths_ConfigModeIsCheckedThroughTheExpansion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no mode bits to check; the file's ACL is the user's")
	}
	cfgs, routes := tildeHost(t)
	if _, err := loadRouted(routes); err != nil {
		t.Fatalf("control: a private config reached through ~/ must load: %v", err)
	}
	if err := os.Chmod(cfgs["personal"].Path(), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadRouted(routes)
	if err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("a 0644 config.json behind a ~/ path must be refused, got %v", err)
	}
}

func TestTildePaths_ALiteralTildeIsAnErrorNotASkip(t *testing.T) {
	tildeHost(t)
	lit := "~/.claudesync.d/personal/config.json"
	if _, err := config.Load(lit); err == nil {
		t.Error("config.Load of an unexpanded ~/ path must fail")
	}
	if _, err := loadConfig(lit); err == nil {
		t.Error("loadConfig of an unexpanded ~/ path must fail")
	}
	if err := configTrusted(config.Config{ConfigPath: lit}); err == nil {
		t.Error("configTrusted of an unexpanded ~/ path must fail")
	}
}

// The trust check also fires for a config reached through ~/.
func TestTildePaths_TrustCheckSeesATrackedSpelling(t *testing.T) {
	cfgs, routes := tildeHost(t)
	trackedAsOtherCase(t, cfgs["personal"].SyncDir)
	set, err := loadRouted(routes)
	if err != nil {
		t.Fatal(err)
	}
	cfg := set.Cfgs["personal"]
	if err := configTrusted(cfg); err == nil || !strings.Contains(err.Error(), "tracked") {
		t.Fatalf("got %v", err)
	}
	if filepath.Base(cfg.ConfigPath) != "config.json" || strings.HasPrefix(cfg.ConfigPath, "~") {
		t.Fatalf("config path was not expanded: %q", cfg.ConfigPath)
	}
}

func TestConfigTrusted_RefusesAnUnexpandedTildeEvenIfADirCalledTildeExists(t *testing.T) {
	d := t.TempDir()
	os.MkdirAll(filepath.Join(d, "~"), 0o700)
	os.WriteFile(filepath.Join(d, "~", "config.json"), []byte(`{}`), 0o600)
	t.Chdir(d)
	err := configTrusted(config.Config{ConfigPath: "~/config.json"})
	if err == nil || !strings.Contains(err.Error(), "not expanded") {
		t.Errorf("got %v", err)
	}
}
