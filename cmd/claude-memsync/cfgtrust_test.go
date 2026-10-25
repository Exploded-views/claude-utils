package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	syncpkg "github.com/MarimerLLC/claude-utils/internal/sync"
)

// trustFixture: a guard config whose githubTokenCmd is a marker-writing script,
// saved to config.json (0600, untracked) and reloaded like the daemon does.
func trustFixture(t *testing.T) (cfg config.Config, marker string) {
	t.Helper()
	cfg = guardCfg(t, okRepoHandler(new([]string)), false)
	marker = filepath.Join(t.TempDir(), "ran")
	cfg.GitHubTokenCmd = []string{tokScript(t, "touch "+marker+"; echo "+cmdTokSecret)}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(cfg.Path())
	if err != nil {
		t.Fatal(err)
	}
	return loaded, marker
}

func warnings(t *testing.T) *[]string {
	t.Helper()
	var w []string
	old := warnf
	warnf = func(f string, a ...any) { w = append(w, f) }
	t.Cleanup(func() { warnf = old })
	resetTokenCmdWarned()
	return &w
}

func ran(marker string) bool { _, err := os.Stat(marker); return err == nil }

func TestTokenCmdFromTrustedConfigRuns(t *testing.T) {
	cfg, marker := trustFixture(t)
	w := warnings(t)
	c, err := guardClient(cfg)
	if err != nil || c == nil || !ran(marker) {
		t.Fatalf("a 0600, untracked, own config.json must be honoured: %v %v ran=%v", c, err, ran(marker))
	}
	if len(*w) != 0 {
		t.Errorf("unexpected warning: %v", *w)
	}
}

func TestTokenCmdIgnoredWhenConfigModeLoose(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o660, 0o666, 0o602} {
		cfg, marker := trustFixture(t)
		w := warnings(t)
		if err := os.Chmod(cfg.ConfigPath, mode); err != nil {
			t.Fatal(err)
		}
		c, err := guardClient(cfg)
		if err != nil || c != nil || ran(marker) {
			t.Fatalf("mode %o: token command must be ignored: client=%v err=%v ran=%v", mode, c, err, ran(marker))
		}
		if len(*w) == 0 || !strings.Contains((*w)[0], "githubTokenCmd") {
			t.Errorf("mode %o: want a warning, got %v", mode, *w)
		}
	}
}

// A remote that tracks config.json carries its own githubTokenCmd; even if it
// got into the work-tree, nothing runs while HEAD tracks the file.
func TestTokenCmdIgnoredWhenConfigIsTrackedAtHead(t *testing.T) {
	cfg, marker := trustFixture(t)
	w := warnings(t)
	repo := gitwt.New(cfg.SyncDir)
	if _, err := repo.Run("add", "-f", "config.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Run("commit", "-q", "-m", "hostile config"); err != nil {
		t.Fatal(err)
	}
	c, err := guardClient(cfg)
	if err != nil || c != nil || ran(marker) {
		t.Fatalf("tracked config.json: token command must not run: %v %v ran=%v", c, err, ran(marker))
	}
	if len(*w) == 0 {
		t.Error("want a warning")
	}
}

func TestTokenCmdIgnoredWhenConfigIsStagedOrMissing(t *testing.T) {
	cfg, marker := trustFixture(t)
	warnings(t)
	repo := gitwt.New(cfg.SyncDir)
	repo.Run("add", "-f", "config.json") // tracked in the index, not yet committed
	if c, _ := guardClient(cfg); c != nil || ran(marker) {
		t.Fatal("an indexed config.json is tracked")
	}
	repo.Run("rm", "-q", "--cached", "config.json")
	os.Remove(cfg.ConfigPath)
	if c, _ := guardClient(cfg); c != nil || ran(marker) {
		t.Fatal("no config file on disk: the command did not come from one")
	}
}

func TestTokenCmdFlagIsAlwaysHonoured(t *testing.T) {
	cfg, marker := trustFixture(t)
	warnings(t)
	os.Chmod(cfg.ConfigPath, 0o666)
	cfg.GitHubTokenCmd = nil
	applyTokenCmdFlag(&cfg, tokScript(t, "touch "+marker+"; echo "+cmdTokSecret))
	if c, err := guardClient(cfg); err != nil || c == nil || !ran(marker) {
		t.Fatalf("a command typed on this command line is trusted: %v %v", c, err)
	}
}

func TestTokenCmdOutsideWorkTreeOnlyNeedsOwnerAndMode(t *testing.T) {
	cfg, marker := trustFixture(t)
	warnings(t)
	elsewhere := filepath.Join(t.TempDir(), "cfg.json")
	b, _ := os.ReadFile(cfg.ConfigPath)
	os.WriteFile(elsewhere, b, 0o600)
	cfg.ConfigPath = elsewhere
	// even a tracked config.json in the sync dir is irrelevant to this file
	gitwt.New(cfg.SyncDir).Run("add", "-f", "config.json")
	if c, _ := guardClient(cfg); c == nil || !ran(marker) {
		t.Fatal("a config outside the work-tree is judged on its own mode and owner")
	}
}

func TestStatusShowsRefusedPull(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".state"), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, ".state", "pull-refused"), []byte("the remote tracks per-PC files (config.json)\n"), 0o600)
	if l := syncpkg.PullRefusedLine(dir); !strings.Contains(l, "pull refused") {
		t.Errorf("%q", l)
	}
}
