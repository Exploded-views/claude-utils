package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/config"
)

const cmdTokSecret = "ghp_TOKENCMD_SECRET_55555"

func tokScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gh-token.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	return p
}

func okRepoHandler(seen *[]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r.Header.Get("Authorization"))
		if r.Method == "POST" {
			w.WriteHeader(201)
		}
		_ = json.NewEncoder(w).Encode(repoJSON{Name: "mem-personal", Private: tb(true), Visibility: "private"})
	}
}

func TestRepoCreateWithTokenCmdFlag(t *testing.T) {
	var seen []string
	cfg := guardCfg(t, okRepoHandler(&seen), false)
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	script := tokScript(t, "echo "+cmdTokSecret)
	var out, errb bytes.Buffer
	// drive through the same flag handling runRepo uses
	applyTokenCmdFlag(&cfg, script)
	if code := repoCreateRun(cfg, "Org", "mem-personal", &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if len(seen) == 0 || !strings.Contains(seen[0], cmdTokSecret) {
		t.Fatalf("API never saw the command's token: %v", seen)
	}
	if strings.Contains(out.String()+errb.String(), cmdTokSecret) {
		t.Error("token printed")
	}
	b, _ := os.ReadFile(cfg.Path())
	if strings.Contains(string(b), cmdTokSecret) {
		t.Error("token written to config")
	}
}

func TestDoctorTokenCmdFlag(t *testing.T) {
	var seen []string
	cfg := guardCfg(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		orgList(nil)(w, r)
	}, false)
	cfg.RemoteURL = ""
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	script := tokScript(t, "echo "+cmdTokSecret)
	var out, errb bytes.Buffer
	if code := doctorCmd(&out, &errb, []string{"--config", cfg.Path(), "--org", "Org", "--token-cmd", script}); code != 0 {
		t.Fatalf("exit %d: %s %s", code, out.String(), errb.String())
	}
	if len(seen) == 0 || !strings.Contains(seen[0], cmdTokSecret) {
		t.Fatalf("token not used: %v", seen)
	}
	if strings.Contains(out.String()+errb.String(), cmdTokSecret) {
		t.Error("token printed")
	}
}

func TestTokenCmdFailureIsNoToken(t *testing.T) {
	var seen []string
	cfg := guardCfg(t, okRepoHandler(&seen), false)
	cfg.GitHubTokenCmd = []string{tokScript(t, "echo "+cmdTokSecret+"; echo "+cmdTokSecret+" >&2; exit 1")}
	cfg = savedAndLoaded(t, cfg)
	var out, errb bytes.Buffer
	if code := repoCreateRun(cfg, "Org", "mem-personal", &out, &errb); code == 0 {
		t.Fatal("want failure without a token")
	}
	if len(seen) != 0 {
		t.Error("API must not be called without a token")
	}
	if !strings.Contains(errb.String(), "no GitHub token") {
		t.Errorf("want the existing no-token message: %q", errb.String())
	}
	if strings.Contains(out.String()+errb.String(), cmdTokSecret) {
		t.Error("command output leaked into errors")
	}
}

func TestTokenFileWinsOverCmd(t *testing.T) {
	var seen []string
	cfg := guardCfg(t, okRepoHandler(&seen), true)
	cfg.GitHubTokenCmd = []string{tokScript(t, "echo "+cmdTokSecret)}
	cfg = savedAndLoaded(t, cfg)
	c, err := guardClient(cfg)
	if err != nil || c == nil {
		t.Fatal(err)
	}
	if _, err := c.GetRepo("Org", "mem-personal"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(seen[0], cmdTok) || strings.Contains(seen[0], cmdTokSecret) {
		t.Errorf("token file must win: %v", seen)
	}
}

func TestDaemonRecheckUsesTokenCmdAndNeverLogsIt(t *testing.T) {
	var seen []string
	cfg := guardCfg(t, okRepoHandler(&seen), false)
	cfg.GitHubTokenCmd = []string{tokScript(t, "echo "+cmdTokSecret)}
	cfg = savedAndLoaded(t, cfg)
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, f) }
	allowed := pushWatch(cfg, logf)
	if allowed == nil || !allowed() {
		t.Fatal("watch should allow a private repo")
	}
	if len(seen) == 0 || !strings.Contains(seen[0], cmdTokSecret) {
		t.Fatalf("recheck did not use the token command: %v", seen)
	}
	if strings.Contains(strings.Join(logs, "\n"), cmdTokSecret) {
		t.Error("token logged")
	}
}

func TestRecheckTokenCmdFalseSkipsCommand(t *testing.T) {
	var seen []string
	cfg := guardCfg(t, okRepoHandler(&seen), false)
	marker := filepath.Join(t.TempDir(), "ran")
	cfg.GitHubTokenCmd = []string{tokScript(t, "touch "+marker+"; echo "+cmdTokSecret)}
	f := false
	cfg.RecheckTokenCmd = &f
	cfg = savedAndLoaded(t, cfg)
	allowed := pushWatch(cfg, func(string, ...any) {})
	if allowed == nil {
		t.Fatal("local checks still apply")
	}
	allowed()
	if err := daemonGuard(cfg, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("token command ran although recheckTokenCmd is false")
	}
	if len(seen) != 0 {
		t.Errorf("API called without a token: %v", seen)
	}
}

func TestConfigRoundTripTokenCmd(t *testing.T) {
	initEnv(t)
	t.Setenv("HOME", t.TempDir())
	cfg := config.Defaults()
	cfg.SyncDir = t.TempDir()
	cfg.GitHubTokenCmd = []string{"gh", "auth", "token"}
	f := false
	cfg.RecheckTokenCmd = &f
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	got, err := config.Load(cfg.Path())
	if err != nil || len(got.GitHubTokenCmd) != 3 || got.RecheckTokenCmd == nil || *got.RecheckTokenCmd {
		t.Fatalf("round trip: %+v %v", got, err)
	}
}

// savedAndLoaded writes cfg to its config.json (0600) and reads it back, the
// way the daemon gets a config whose githubTokenCmd it may honour.
func savedAndLoaded(t *testing.T, cfg config.Config) config.Config {
	t.Helper()
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(cfg.Path())
	if err != nil {
		t.Fatal(err)
	}
	return c
}
