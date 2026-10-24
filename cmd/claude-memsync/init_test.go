package main

import (
	"os/exec"
	"bytes"
	"os"
	"path/filepath"
	"strings"
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

// With --force an existing config that cannot be loaded cannot supply the
// projectKey to keep. The fallback must be announced, not applied silently.
func TestResolveProjectKey_WarnsWhenTheExistingConfigCannotBeLoaded(t *testing.T) {
	var out bytes.Buffer
	old := stderr
	stderr = &out
	defer func() { stderr = old }()

	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.SyncDir = dir

	// No config at all is a fresh init: nothing to warn about.
	if got, err := resolveProjectKey("", true, cfg); err != nil || got != "path" || out.Len() != 0 {
		t.Errorf("missing config: %q, %v, warning %q", got, err, out.String())
	}

	if err := os.WriteFile(cfg.Path(), []byte(`{"projectKey": "remote",`), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := resolveProjectKey("", true, cfg)
	if err != nil || got != "path" {
		t.Errorf("unloadable config: %q, %v", got, err)
	}
	for _, want := range []string{"warning", cfg.Path(), `"path"`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("warning %q should mention %s", out.String(), want)
		}
	}

	// An explicit flag needs no fallback and says nothing.
	out.Reset()
	if got, _ := resolveProjectKey("remote", true, cfg); got != "remote" || out.Len() != 0 {
		t.Errorf("flag: %q, warning %q", got, out.String())
	}
}

// Local conflict backups never reach the sync repo: the one a join makes
// (.local-before-join-*) as much as the one Reconcile makes (.from-remote-*).
func TestWriteGitignore_ExcludesLocalBackups(t *testing.T) {
	dir := t.TempDir()
	if err := writeGitignore(dir); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"*.from-remote-*", "*.local-before-join-*"} {
		if !strings.Contains(string(b), want+"\n") {
			t.Errorf(".gitignore lacks %q:\n%s", want, b)
		}
	}
}

// init records the mode in .state/mode, and init --force into the same sync dir
// with another mode is refused, as is a sync dir from before modes were
// recorded (those were path-mode ones) asked to be remote.
func TestBootstrap_RecordsTheModeAndRefusesAChange(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", "-b", "main", origin).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	cfg := config.Defaults()
	cfg.SyncDir = filepath.Join(root, "sync")
	cfg.ClaudeProjectsDir = filepath.Join(root, "claude", "projects")
	cfg.RemoteURL = origin
	cfg.MergeDriverPath = "claude-memmerge"
	cfg.ProjectKey = config.ProjectKeyRemote
	modeFile := filepath.Join(cfg.SyncDir, ".state", "mode")

	if err := bootstrap(cfg, false); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(modeFile); strings.TrimSpace(string(b)) != "remote" {
		t.Fatalf("mode file = %q", b)
	}
	if err := bootstrap(cfg, true); err != nil {
		t.Errorf("--force with the same mode: %v", err)
	}
	other := cfg
	other.ProjectKey = config.ProjectKeyPath
	if err := bootstrap(other, true); err == nil || !strings.Contains(err.Error(), "fresh sync dir") {
		t.Errorf("--force with another mode = %v; want a refusal naming a fresh sync dir", err)
	}
	if b, _ := os.ReadFile(modeFile); strings.TrimSpace(string(b)) != "remote" {
		t.Errorf("a refused init changed the record: %q", b)
	}

	if err := os.Remove(modeFile); err != nil { // as a sync dir made by an older version
		t.Fatal(err)
	}
	if err := bootstrap(cfg, true); err == nil || !strings.Contains(err.Error(), "fresh sync dir") {
		t.Errorf("--force remote into a sync dir with no record = %v; want a refusal", err)
	}
	if err := bootstrap(other, true); err != nil {
		t.Errorf("--force path into a sync dir with no record: %v", err)
	}
	if b, _ := os.ReadFile(modeFile); strings.TrimSpace(string(b)) != "path" {
		t.Errorf("mode file after a path init = %q", b)
	}
}
