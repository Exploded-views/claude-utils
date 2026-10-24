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
	// Nothing here may reach the real ~/.claudesync or ~/.claude, and the
	// hook's project default must not depend on the caller's environment.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_PROJECT_DIR", "")

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
	code := changesCmd(nil, &out, &errb, []string{"--config", cfgPath, "--since", base, "--json", "--project", "/home/alice/src/app"})
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
	if code := changesCmd(nil, &out, &errb, []string{"--config", cfgPath, "--since", base}); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "b.md") || !strings.Contains(out.String(), "2 memory files changed") {
		t.Errorf("text output: %q", out.String())
	}
}

func TestChangesCmd_Limit(t *testing.T) {
	cfgPath, base := changesRepo(t)
	var out, errb bytes.Buffer
	if code := changesCmd(nil, &out, &errb, []string{"--config", cfgPath, "--since", base, "--limit", "1"}); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "b.md") || strings.Contains(out.String(), "c.md") || !strings.Contains(out.String(), "...1 more") {
		t.Errorf("text output: %q", out.String())
	}
}

func TestChangesCmd_Usage(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	cases := [][]string{
		{"--config", cfgPath},                               // --since or --marker is required
		{"--config", cfgPath, "--since", "HEAD", "--bogus"}, // unknown flag
		{"--config", cfgPath, "--since", "HEAD", "--limit", "-1"},
		{"--config", cfgPath, "--since", "HEAD", "--update-marker"}, // needs --marker
		{"--config", cfgPath, "--since", "HEAD", "--marker", "m"},   // exclusive
		{"--config", cfgPath, "--update-marker"},                    // neither
	}
	for _, args := range cases {
		var out, errb bytes.Buffer
		if code := changesCmd(nil, &out, &errb, args); code == 0 {
			t.Errorf("%v: want non-zero exit", args)
		}
	}
}

func TestChangesCmd_MarkerRoundTrip(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	marker := filepath.Join(t.TempDir(), "seen")
	args := []string{"--config", cfgPath, "--marker", marker, "--update-marker", "--json"}

	var out, errb bytes.Buffer
	if code := changesCmd(nil, &out, &errb, args); code != 0 {
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

func TestChangesCmd_BadSinceWithUpdateMarkerIsAnError(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	dir := t.TempDir()
	wd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	var out, errb bytes.Buffer
	if code := changesCmd(nil, &out, &errb, []string{"--config", cfgPath, "--since", "HEAD~5", "--update-marker"}); code == 0 {
		t.Error("want a usage error")
	}
	if code := changesCmd(nil, &out, &errb, []string{"--config", cfgPath, "--since", "HEAD~5"}); code != 1 {
		t.Errorf("bad revision: exit %d, want 1", code)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("stray files: %v", entries)
	}
}

func TestChangesCmd_MarkerReset(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	marker := filepath.Join(t.TempDir(), "seen")
	if err := os.WriteFile(marker, []byte(strings.Repeat("ab", 20)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--config", cfgPath, "--marker", marker, "--update-marker", "--json"}
	var out, errb bytes.Buffer
	if code := changesCmd(nil, &out, &errb, args); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	var r struct {
		Reset bool
		Total int
	}
	if err := json.Unmarshal(out.Bytes(), &r); err != nil || !r.Reset || r.Total != 0 {
		t.Errorf("want reset with nothing reported: %s", out.String())
	}
	if n := strings.Count(strings.TrimSpace(errb.String()), "\n") + 1; n != 1 || !strings.Contains(errb.String(), "reset") {
		t.Errorf("want exactly one warning line, got %q", errb.String())
	}
	// Without --update-marker a stale marker stays an error.
	if err := os.WriteFile(marker, []byte(strings.Repeat("ab", 20)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errb.Reset()
	if code := changesCmd(nil, &out, &errb, []string{"--config", cfgPath, "--marker", marker}); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
}

// --update-marker pointed at a note must leave it alone and say so without
// echoing it.
func TestChangesCmd_UpdateMarkerNeverOverwritesANote(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	const note = "# My notes\nimportant-4c1d\n"
	marker := filepath.Join(t.TempDir(), "MEMORY.md")
	if err := os.WriteFile(marker, []byte(note), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := changesCmd(nil, &out, &errb, []string{"--config", cfgPath, "--marker", marker, "--update-marker"}); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if b, _ := os.ReadFile(marker); string(b) != note {
		t.Errorf("note was overwritten: %q", b)
	}
	if out.Len() != 0 || strings.Contains(errb.String(), "important-4c1d") || !strings.Contains(errb.String(), "does not contain a commit id") {
		t.Errorf("stdout %q stderr %q", out.String(), errb.String())
	}
}

// The CLI compares with HEAD unless --synced asks for what was copied into
// ~/.claude.
func TestChangesCmd_Synced(t *testing.T) {
	cfgPath, base := changesRepo(t)
	cfg, _ := loadDistillConfig(cfgPath)
	writeSyncedHead(t, cfg.SyncDir, base)

	run := func(args ...string) (int, string) {
		var out, errb bytes.Buffer
		code := changesCmd(strings.NewReader(""), &out, &errb, append([]string{"--config", cfgPath, "--since", base, "--json"}, args...))
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errb.String())
		}
		var res struct {
			Total int    `json:"total"`
			Head  string `json:"head"`
		}
		if err := json.Unmarshal(out.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		return res.Total, res.Head
	}
	if total, _ := run(); total != 2 {
		t.Errorf("default compares with HEAD: total = %d, want 2", total)
	}
	if total, head := run("--synced"); total != 0 || head != base {
		t.Errorf("--synced: total = %d head = %s, want 0 and %s", total, head, base)
	}
}
