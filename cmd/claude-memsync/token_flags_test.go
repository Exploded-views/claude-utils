package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/config"
)

// A host whose config.json names a githubApp (say the Metadata-only check app)
// must still use the token the operator types on the command line.
func TestExplicitTokenFlagsBeatConfigApp(t *testing.T) {
	f, cfg := hostFixture(t)
	f.repos = []string{"mem-personal"}
	cfg.GitHubTokenFile = ""
	cfg.GitHubApp = "check-app-not-in-apps-json" // unusable: proves it is not consulted
	cfg.SyncDir = t.TempDir()
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	tokFile := filepath.Join(t.TempDir(), "admin-token")
	if err := os.WriteFile(tokFile, []byte("placeholder-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// without a flag the config's app is used (and fails here)
	var out, errb bytes.Buffer
	if code := hostCmd(&out, &errb, []string{"list", "--config", cfg.Path()}); code == 0 {
		t.Fatal("setup: the config's app must be what a flagless run uses")
	}
	for name, flags := range map[string][]string{
		"--token-file": {"--token-file", tokFile},
		"--token-cmd":  {"--token-cmd", "printf %s placeholder-token"},
	} {
		out.Reset()
		errb.Reset()
		code := hostCmd(&out, &errb, append([]string{"list", "--config", cfg.Path()}, flags...))
		if code != 0 {
			t.Errorf("host list %s: exit %d: %s", name, code, errb.String())
		}
		out.Reset()
		errb.Reset()
		code = hostCmd(&out, &errb, append([]string{"retire", "vm-1", "--config", cfg.Path()}, flags...))
		if strings.Contains(errb.String(), "check-app-not-in-apps-json") || strings.Contains(errb.String(), "apps.json") {
			t.Errorf("host retire %s consulted the config's app: %s", name, errb.String())
		}
		_ = code
	}
}

func TestDoctorAndRepoCreateFlagsBeatConfigApp(t *testing.T) {
	var cfg config.Config
	cfg.GitHubApp = "check-app"
	cfg.GitHubTokenFile = "/x/file"
	tokenFlags{Cmd: "printf %s tok"}.apply(&cfg)
	if cfg.GitHubApp != "" || cfg.GitHubTokenFile != "" || len(cfg.GitHubTokenCmd) == 0 || !cfg.FromFlag("githubTokenCmd") {
		t.Errorf("--token-cmd must replace config sources: %+v", cfg)
	}
	cfg = config.Config{GitHubApp: "check-app", GitHubTokenCmd: []string{"x"}}
	tokenFlags{File: "/y/file"}.apply(&cfg)
	if cfg.GitHubApp != "" || len(cfg.GitHubTokenCmd) != 0 || cfg.GitHubTokenFile != "/y/file" || !cfg.FromFlag("githubTokenFile") {
		t.Errorf("--token-file must replace config sources: %+v", cfg)
	}
	cfg = config.Config{GitHubTokenFile: "/z", GitHubTokenCmd: []string{"x"}}
	tokenFlags{App: "admin-app"}.apply(&cfg)
	if cfg.GitHubApp != "admin-app" || cfg.GitHubTokenFile != "" || len(cfg.GitHubTokenCmd) != 0 {
		t.Errorf("--github-app must replace config sources: %+v", cfg)
	}
	cfg = config.Config{GitHubApp: "check-app"}
	tokenFlags{}.apply(&cfg)
	if cfg.GitHubApp != "check-app" {
		t.Error("no flag leaves the config alone")
	}
	cfg = config.Config{GitHubApp: "check-app"}
	applyTokenCmdFlag(&cfg, "printf %s tok") // repo create
	if cfg.GitHubApp != "" {
		t.Error("repo create --token-cmd must beat the config's app")
	}
}

// init saves --github-app to the config; --token-cmd would silently drop it.
func TestInit_RefusesGitHubAppWithTokenCmd(t *testing.T) {
	var code int
	out := captureStderr(t, func() {
		code = runInit([]string{"--remote", "/nonexistent/x.git", "--github-app", "chk", "--token-cmd", "printf x"})
	})
	if code != 2 || !strings.Contains(out, "--github-app") || !strings.Contains(out, "--token-cmd") {
		t.Fatalf("exit %d, %q", code, out)
	}
}
