package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/secretcmd"
)

// tokenCmdHost is a related host whose primary repo gets its token from a
// command that records that it ran (and then dawdles). The returned marker
// exists only if the command was started.
func tokenCmdHost(t *testing.T) (h *relHost, marker string) {
	t.Helper()
	api := newFakeAPI(t)
	bare := bareRemote(t, "mem-project-b", map[string]string{"projects/p/memory/n.md": "one"})
	h = visHost(t, api, map[string]string{"mem-project-b": bare})
	if code, _, errs := relate("mem-project-a", "mem-project-b", "--read"); code != 0 {
		t.Fatalf("relate: %s", errs)
	}
	marker = filepath.Join(t.TempDir(), "token-cmd-ran")
	script := tokScript(t, "echo ran >> '"+marker+"'\nsleep 5\necho tok")
	t.Setenv("MEMSYNC_GITHUB_TOKEN", "")
	cfg := h.cfgs["a"]
	cfg.GitHubTokenCmd = []string{script}
	cfg.GitHubAPIURL, cfg.GitHubHost, cfg.GitHubOrg = api.srv.URL, "github.com", "Org"
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	// Verdicts are stale: past the cap, so only a network check could help.
	rc, err := loadRelCtx("")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	b, _ := json.Marshal(map[string]verdict{
		"org/mem-project-a": {Private: true, At: old},
		"org/mem-project-b": {Private: true, At: old},
	})
	if err := os.MkdirAll(filepath.Dir(rc.vis.cachePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rc.vis.cachePath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	os.Remove(marker)
	return h, marker
}

func wantDaemonSecrets(t *testing.T) {
	t.Helper()
	if got := secretcmd.KeyCmdTimeout(); got != secretcmd.DefaultTimeout {
		t.Errorf("secret timeout = %v, want the short daemon timeout %v", got, secretcmd.DefaultTimeout)
	}
}

func TestHook_NeverRunsTokenCmdOrWaits(t *testing.T) {
	_, marker := tokenCmdHost(t)
	t.Setenv(secretcmd.TimeoutEnv, "")
	secretcmd.SetDaemon(false)
	t.Cleanup(func() { secretcmd.SetDaemon(false) })
	start := time.Now()
	code, out, errs := runHook(t, hookJSON("s1", "/proj/x", "UserPromptSubmit"), "--hook", "--project", "/proj/x", "--deadline", "4s")
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("the hook took %v; it must not wait on a token command", el)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the hook ran the token command")
	}
	if code != 0 {
		t.Fatalf("exit %d, out %q err %q", code, out, errs)
	}
	if strings.Count(errs, "not shown this time") != 1 {
		t.Errorf("want exactly one stderr line saying the related repo is not shown this time:\n%s", errs)
	}
	wantDaemonSecrets(t)
}

func TestRelatedContext_HookNeverRunsTokenCmdOrWaits(t *testing.T) {
	_, marker := tokenCmdHost(t)
	t.Setenv(secretcmd.TimeoutEnv, "")
	secretcmd.SetDaemon(false)
	t.Cleanup(func() { secretcmd.SetDaemon(false) })
	start := time.Now()
	code, out, errs := contextFor("/proj/x")
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("related context took %v; it must not wait on a token command", el)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("related context ran the token command")
	}
	if code != 0 {
		t.Fatalf("exit %d, out %q err %q", code, out, errs)
	}
	wantDaemonSecrets(t)
}

// A non-hook command with fresh verdicts needs no client, so no token command.
func TestVis_ClientIsBuiltOnlyWhenAVerdictIsNeeded(t *testing.T) {
	_, marker := tokenCmdHost(t)
	rc, err := loadRelCtx("")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]verdict{
		"org/mem-project-a": {Private: true, At: time.Now()},
		"org/mem-project-b": {Private: true, At: time.Now()},
	})
	if err := os.WriteFile(rc.vis.cachePath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rc.vis.CheckPrivate("mem-project-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a fresh cached verdict still ran the token command")
	}
}

func TestDoctor_UnattendedFlagAndTimerUnit(t *testing.T) {
	t.Cleanup(func() { secretcmd.SetDaemon(false) })
	secretcmd.SetDaemon(false)
	t.Setenv(secretcmd.TimeoutEnv, "")
	var out, errb strings.Builder
	doctorCmd(&out, &errb, []string{"--unattended", "--org", "Org", "--config", filepath.Join(t.TempDir(), "none.json")})
	if strings.Contains(errb.String(), "flag provided but not defined") {
		t.Fatalf("doctor has no --unattended flag:\n%s", errb.String())
	}
	wantDaemonSecrets(t)
	var o, e strings.Builder
	unitDir := t.TempDir()
	if code := installTimerRun("linux", "/bin/claude-memsync", "Org", "/c/config.json", "/c/tok", unitDir, &o, &e); code != 0 {
		t.Fatalf("install: %s", e.String())
	}
	b, err := os.ReadFile(filepath.Join(unitDir, "claude-memsync-doctor.service"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "--unattended") {
		t.Errorf("the timer unit must run doctor --unattended:\n%s", b)
	}
}
