package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/config"
)

func initEnv(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
}

// initFixture is a Config over temp dirs, a local bare origin and a Claude
// tree holding two memory files.
func initFixture(t *testing.T) (config.Config, string) {
	t.Helper()
	initEnv(t)
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", "-b", "main", origin).CombinedOutput(); err != nil {
		t.Fatalf("init bare: %v: %s", err, out)
	}
	claude := filepath.Join(root, "claude", "projects")
	mem := filepath.Join(claude, "p1", "memory")
	if err := os.MkdirAll(mem, 0700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"clients.md": "about Project-Nightjar\n", "ok.md": "harmless\n"} {
		if err := os.WriteFile(filepath.Join(mem, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Defaults()
	cfg.SyncDir = filepath.Join(root, "sync")
	cfg.ClaudeProjectsDir = claude
	cfg.RemoteURL = origin
	cfg.MergeDriverPath = "/nonexistent/claude-memmerge"
	return cfg, origin
}

func originFiles(t *testing.T, origin string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", origin, "ls-tree", "-r", "--name-only", "main").CombinedOutput()
	if err != nil {
		t.Fatalf("ls-tree: %v: %s", err, out)
	}
	return string(out)
}

// --denylist is in place before the first CommitMirror, so the bootstrap
// commit already honors it.
func TestBootstrap_DenylistAppliesToFirstCommit(t *testing.T) {
	cfg, origin := initFixture(t)
	if err := bootstrap(cfg, false, []byte("\xef\xbb\xbfproject-nightjar\n")); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(cfg.DenylistPath()); err != nil || !strings.Contains(string(got), "project-nightjar") {
		t.Errorf("denylist not installed: %q %v", got, err)
	}
	if fi, err := os.Stat(cfg.DenylistPath()); err == nil && fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("denylist should be private, mode %v", fi.Mode())
	}
	files := originFiles(t, origin)
	if strings.Contains(files, "clients.md") {
		t.Errorf("denylisted file was pushed by init:\n%s", files)
	}
	if !strings.Contains(files, "ok.md") || strings.Contains(files, "denylist.txt") {
		t.Errorf("unexpected pushed files:\n%s", files)
	}
}

func TestBootstrap_WithoutDenylistCommitsEverything(t *testing.T) {
	cfg, origin := initFixture(t)
	if err := bootstrap(cfg, false, nil); err != nil {
		t.Fatal(err)
	}
	if files := originFiles(t, origin); !strings.Contains(files, "clients.md") {
		t.Errorf("expected clients.md pushed without a denylist:\n%s", files)
	}
	if _, err := os.Stat(cfg.DenylistPath()); err == nil {
		t.Error("no denylist should be created without --denylist")
	}
}

func TestReadDenylistFlag(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if b, err := readDenylistFile(write("ok.txt", "# c\nwidget\n")); err != nil || len(b) == 0 {
		t.Errorf("valid file: %v", err)
	}
	if _, err := readDenylistFile(write("bad.txt", "(unclosed\n")); err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Errorf("invalid regex should fail naming the line, got %v", err)
	}
	if _, err := readDenylistFile(write("u16.txt", "\xff\xfew\x00")); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Errorf("UTF-16 should fail, got %v", err)
	}
	if _, err := readDenylistFile(filepath.Join(dir, "missing.txt")); err == nil {
		t.Error("missing file should fail")
	}
}

// status says so when syncing is paused for a rebase being resolved by hand, and
// says nothing otherwise.
func TestPausedLine(t *testing.T) {
	cfg, _ := initFixture(t)
	if got := pausedLine(cfg); got != "" {
		t.Fatalf("paused with nothing stopped: %q", got)
	}
	dir := filepath.Join(cfg.SyncDir, ".state")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "paused"), []byte("2026-10-24T04:00:00Z\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got := pausedLine(cfg)
	if !strings.Contains(got, "syncing paused since") || !strings.Contains(got, "rebase --abort") {
		t.Errorf("got %q", got)
	}
}

// Init of one repo in a routed host never commits a folder routed elsewhere.
func TestBootstrap_OwnsGateKeepsOtherReposFoldersOut(t *testing.T) {
	cfg, origin := initFixture(t)
	if err := bootstrapOwned(cfg, false, nil, func(f string) bool { return f == "not-p1" }); err != nil {
		t.Fatal(err)
	}
	if files := originFiles(t, origin); strings.Contains(files, "p1/") {
		t.Fatalf("init committed a folder routed to another repo:\n%s", files)
	}
	if m, err := os.ReadFile(filepath.Join(cfg.SyncDir, ".state", "manifest.json")); err != nil || strings.Contains(string(m), "p1/") {
		t.Fatalf("manifest lists an unowned folder: %q %v", m, err)
	}
}

func TestInitOwner_RefusesSyncDirTheRoutesDoNotList(t *testing.T) {
	cfgs := routedHost(t, []string{"projects"}, "", []map[string]string{{"folder": "-a", "repo": "projects"}})
	owns, err := initOwner(cfgs["projects"])
	if err != nil || owns == nil || !owns("-a") || owns("-b") {
		t.Fatalf("listed sync dir: owns=%v err=%v", owns != nil, err)
	}
	other := cfgs["projects"]
	other.SyncDir = filepath.Join(t.TempDir(), "elsewhere")
	if _, err := initOwner(other); err == nil || !strings.Contains(err.Error(), "does not list") {
		t.Fatalf("unlisted sync dir: %v", err)
	}
}

func TestInitOwner_NoRoutesFileMeansNoGate(t *testing.T) {
	cfg, _ := initFixture(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv(routesEnv, "")
	if owns, err := initOwner(cfg); owns != nil || err != nil {
		t.Fatalf("owns=%v err=%v", owns != nil, err)
	}
}
