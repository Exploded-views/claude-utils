package main

import (
	"os"
	"sync/atomic"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/proc"
)

func requireStripped(t *testing.T, what string, c config.Config) {
	t.Helper()
	if len(c.GitHubTokenCmd) > 0 || c.GitHubTokenFile != "" || c.GitHubApp != "" || c.GitHubAPIURL != "" || c.MergeDriverPath != "" {
		t.Errorf("%s: the hook holds a sensitive key: %+v", what, c)
	}
}

// Every way the hook finds its config, not only the routes file, reads it
// without the trust check: no git, and none of the five keys.
func TestHookTargets_EveryConfigSourceIsStrippedAndGitFree(t *testing.T) {
	var n atomic.Int64
	restore := proc.SetSpawnHook(func(string, []string) { n.Add(1) })
	t.Cleanup(restore)

	t.Run("--config named with routes", func(t *testing.T) {
		cfgs, _ := realisticHost(t, 2)
		requireTrusted(t, cfgs)
		n.Store(0)
		tg, err := hookTargets(cfgs["r00"].Path(), "")
		if err != nil || len(tg) != 1 {
			t.Fatalf("%v (%d targets)", err, len(tg))
		}
		requireStripped(t, "--config", tg[0].cfg)
		if n.Load() != 0 {
			t.Errorf("%d git processes", n.Load())
		}
	})

	t.Run("no routes file", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		t.Setenv("XDG_CONFIG_HOME", "")
		t.Setenv(routesEnv, "")
		cfg := config.Defaults()
		cfg.SyncDir = t.TempDir()
		cfg.MergeDriverPath = "/opt/bin/claude-memmerge"
		cfg.GitHubTokenCmd = []string{"/bin/echo", "t"}
		if err := config.Save(cfg); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(cfg.Path(), 0o600); err != nil {
			t.Fatal(err)
		}
		raw, err := config.Load(cfg.Path())
		if err != nil || configTrusted(raw) != nil {
			t.Fatalf("fixture config is not trusted: %v", err)
		}
		n.Store(0)
		tg, err := hookTargets(cfg.Path(), "")
		if err != nil || len(tg) != 1 {
			t.Fatalf("%v (%d targets)", err, len(tg))
		}
		requireStripped(t, "default repo", tg[0].cfg)
		if n.Load() != 0 {
			t.Errorf("%d git processes", n.Load())
		}
	})
}
