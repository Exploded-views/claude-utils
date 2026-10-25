package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// keyCase sets one sensitive key in cfg and reports whether it is still set.
type keyCase struct {
	name string
	set  func(*config.Config)
	got  func(config.Config) bool
}

var sensitive = []keyCase{
	{"githubTokenCmd", func(c *config.Config) { c.GitHubTokenCmd = []string{"/bin/true"} }, func(c config.Config) bool { return len(c.GitHubTokenCmd) > 0 }},
	{"githubTokenFile", func(c *config.Config) { c.GitHubTokenFile = "/tmp/planted-token" }, func(c config.Config) bool { return c.GitHubTokenFile != "" }},
	{"githubApp", func(c *config.Config) { c.GitHubApp = "planted" }, func(c config.Config) bool { return c.GitHubApp != "" }},
	{"githubApiUrl", func(c *config.Config) { c.GitHubAPIURL = "https://evil.example/api" }, func(c config.Config) bool { return c.GitHubAPIURL != "" }},
	{"mergeDriverPath", func(c *config.Config) { c.MergeDriverPath = "/tmp/planted-merge" }, func(c config.Config) bool { return c.MergeDriverPath != "" }},
}

// loadedWith writes a config.json carrying only this key and reads it back
// through loadConfig, the way every command does.
func loadedWith(t *testing.T, k keyCase, mode os.FileMode, track bool) config.Config {
	t.Helper()
	initEnv(t)
	t.Setenv("HOME", t.TempDir())
	cfg := config.Defaults()
	cfg.SyncDir = t.TempDir()
	gitInitRemote(t, cfg.SyncDir, "git@github.com:Org/mem-personal.git")
	k.set(&cfg)
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfg.Path(), mode); err != nil {
		t.Fatal(err)
	}
	if track {
		repo := gitwt.New(cfg.SyncDir)
		if _, err := repo.Run("add", "-f", "config.json"); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.Run("commit", "-q", "-m", "planted"); err != nil {
			t.Fatal(err)
		}
	}
	c, err := loadConfig(cfg.Path())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestEveryKeyHonouredFromTrustedConfig(t *testing.T) {
	for _, k := range sensitive {
		w := warnings(t)
		c := loadedWith(t, k, 0o600, false)
		if !k.got(c) {
			t.Errorf("%s: dropped from a trusted config", k.name)
		}
		if len(*w) != 0 {
			t.Errorf("%s: unexpected warning %v", k.name, *w)
		}
	}
}

func TestEveryKeyIgnoredFromLooseConfig(t *testing.T) {
	for _, k := range sensitive {
		for _, mode := range []os.FileMode{0o644, 0o660, 0o602} {
			w := warnings(t)
			c := loadedWith(t, k, mode, false)
			if k.got(c) {
				t.Errorf("%s at mode %o: still honoured", k.name, mode)
			}
			if len(*w) != 1 || !strings.Contains((*w)[0], "config.json is not trusted") {
				t.Errorf("%s at mode %o: want one warning, got %v", k.name, mode, *w)
			}
		}
	}
}

func TestEveryKeyIgnoredFromTrackedConfig(t *testing.T) {
	for _, k := range sensitive {
		w := warnings(t)
		c := loadedWith(t, k, 0o600, true)
		if k.got(c) {
			t.Errorf("%s: honoured although config.json is tracked", k.name)
		}
		if len(*w) != 1 {
			t.Errorf("%s: want one warning, got %v", k.name, *w)
		}
	}
}

func TestUntrustedConfigWarnsOncePerReason(t *testing.T) {
	w := warnings(t)
	cfg := config.Defaults()
	cfg.SyncDir = t.TempDir()
	cfg.GitHubTokenCmd = []string{"/bin/true"}
	cfg.GitHubAPIURL = "https://evil.example/api"
	cfg.MergeDriverPath = "/tmp/x"
	initEnv(t)
	config.Save(cfg)
	os.Chmod(cfg.Path(), 0o666)
	for i := 0; i < 3; i++ {
		loadConfig(cfg.Path())
	}
	if len(*w) != 1 {
		t.Errorf("want exactly one warning, got %d: %v", len(*w), *w)
	}
	if !strings.Contains((*w)[0], "githubTokenCmd") || !strings.Contains((*w)[0], "githubApiUrl") || !strings.Contains((*w)[0], "mergeDriverPath") {
		t.Errorf("warning does not name the keys: %q", (*w)[0])
	}
}

func TestFlagsStillWorkWhenConfigUntrusted(t *testing.T) {
	k := sensitive[1] // githubTokenFile
	warnings(t)
	c := loadedWith(t, k, 0o666, false)
	if k.got(c) {
		t.Fatal("setup: planted key should have been dropped")
	}
	tok := filepath.Join(t.TempDir(), "tok")
	os.WriteFile(tok, []byte("flag-token\n"), 0o600)
	c.GitHubTokenFile = tok
	c.MarkFlag("githubTokenFile")
	if again := sanitizeCfg(c); again.GitHubTokenFile != tok {
		t.Error("a --token-file value must survive sanitizing")
	}
}

func TestPlantedAPIURLNeverReceivesATokenRequest(t *testing.T) {
	var hits atomic.Int32
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer evil.Close()
	warnings(t)
	cfg := guardCfg(t, nil, true) // has a token file
	cfg.GitHubAPIURL = evil.URL
	cfg.GitHubHost = ""
	skipAPIHostCheck = false
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	os.Chmod(cfg.Path(), 0o644)
	loaded, err := loadConfig(cfg.Path())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.GitHubAPIURL != "" {
		t.Fatalf("planted githubApiUrl survived: %q", loaded.GitHubAPIURL)
	}
	_, _ = guardClient(loaded) // must not contact the planted server
	if hits.Load() != 0 {
		t.Errorf("planted API URL received %d requests", hits.Load())
	}
}
