package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// changesRepo builds a config.json that points at a sync repo holding two
// commits, and returns the config path and the first commit's SHA.
func changesRepo(t *testing.T) (cfgPath, base string) {
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

	cfg := config.Defaults()
	cfg.SyncDir = filepath.Join(t.TempDir(), "sync")
	cfg.ClaudeProjectsDir = filepath.Join(t.TempDir(), "projects")
	repo := gitwt.New(cfg.SyncDir)
	if err := repo.Init("main"); err != nil {
		t.Fatal(err)
	}
	commit := func(names ...string) string {
		for _, name := range names {
			p := filepath.Join(cfg.SyncDir, "projects", "-home-alice-src-app", "memory", name)
			if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(name), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := repo.AddAll(); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.Commit("auto: " + strings.Join(names, ",")); err != nil {
			t.Fatal(err)
		}
		out, _ := repo.Run("rev-parse", "HEAD")
		return strings.TrimSpace(out)
	}
	base = commit("a.md")
	commit("b.md", "c.md")
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	return cfg.Path(), base
}

func TestChangesCmd_JSON(t *testing.T) {
	cfgPath, base := changesRepo(t)
	var out, errb bytes.Buffer
	code := changesCmd(&out, &errb, []string{"--config", cfgPath, "--since", base, "--json", "--project", "/home/alice/src/app"})
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errb.String())
	}
	var got struct {
		Head    string `json:"head"`
		Total   int    `json:"total"`
		Changes []struct {
			Status string `json:"status"`
			File   string `json:"file"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out.String())
	}
	if got.Total != 2 || got.Changes[0].File != "b.md" || got.Changes[0].Status != "added" || got.Head == "" {
		t.Errorf("got %+v", got)
	}
	if errb.Len() != 0 {
		t.Errorf("stderr should be empty, got %q", errb.String())
	}
}

func TestChangesCmd_Text(t *testing.T) {
	cfgPath, base := changesRepo(t)
	var out, errb bytes.Buffer
	if code := changesCmd(&out, &errb, []string{"--config", cfgPath, "--since", base}); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "b.md") || !strings.Contains(out.String(), "2 memory files changed") {
		t.Errorf("text output: %q", out.String())
	}
}

func TestChangesCmd_Limit(t *testing.T) {
	cfgPath, base := changesRepo(t)
	var out, errb bytes.Buffer
	if code := changesCmd(&out, &errb, []string{"--config", cfgPath, "--since", base, "--limit", "1"}); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "b.md") || strings.Contains(out.String(), "c.md") || !strings.Contains(out.String(), "...1 more") {
		t.Errorf("text output: %q", out.String())
	}
}

func TestChangesCmd_Usage(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	cases := [][]string{
		{"--config", cfgPath},                               // --since is required
		{"--config", cfgPath, "--since", "HEAD", "--bogus"}, // unknown flag
		{"--config", cfgPath, "--since", "HEAD", "--limit", "-1"},
		{"--config", cfgPath, "--since", "HEAD", "--update-marker"}, // not a marker file
	}
	for _, args := range cases {
		var out, errb bytes.Buffer
		if code := changesCmd(&out, &errb, args); code == 0 {
			t.Errorf("%v: want non-zero exit", args)
		}
	}
}

func TestChangesCmd_MarkerRoundTrip(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	marker := filepath.Join(t.TempDir(), "seen")
	args := []string{"--config", cfgPath, "--since", marker, "--update-marker", "--json"}

	var out, errb bytes.Buffer
	if code := changesCmd(&out, &errb, args); code != 0 {
		t.Fatalf("first run exit %d: %s", code, errb.String())
	}
	var r struct{ Total int }
	if err := json.Unmarshal(out.Bytes(), &r); err != nil || r.Total != 0 {
		t.Errorf("first run should only set the baseline: %s", out.String())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("marker not written: %v", err)
	}
}
