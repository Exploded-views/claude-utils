package main

import (
	"path/filepath"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/config"
)

func TestResolveProjectKey(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.SyncDir = dir

	// No config yet: the default.
	if got, err := resolveProjectKey("", false, cfg); err != nil || got != "path" {
		t.Errorf("fresh init = %q, %v", got, err)
	}
	if got, err := resolveProjectKey("remote", false, cfg); err != nil || got != "remote" {
		t.Errorf("flag = %q, %v", got, err)
	}
	if _, err := resolveProjectKey("both", false, cfg); err == nil {
		t.Error("an unknown --project-key value must be rejected")
	}

	// An existing config says remote.
	old := cfg
	old.ProjectKey = config.ProjectKeyRemote
	if err := config.Save(old); err != nil {
		t.Fatal(err)
	}
	if got, _ := resolveProjectKey("", true, cfg); got != "remote" {
		t.Errorf("--force without the flag should keep the existing setting, got %q", got)
	}
	if got, _ := resolveProjectKey("path", true, cfg); got != "path" {
		t.Errorf("--force with the flag should use the flag, got %q", got)
	}
	if got, _ := resolveProjectKey("", false, cfg); got != "path" {
		t.Errorf("without --force the existing config is not consulted, got %q", got)
	}

	// An unreadable existing config falls back to the default.
	cfg.SyncDir = filepath.Join(dir, "elsewhere")
	if got, err := resolveProjectKey("", true, cfg); err != nil || got != "path" {
		t.Errorf("no config to keep = %q, %v", got, err)
	}
}
