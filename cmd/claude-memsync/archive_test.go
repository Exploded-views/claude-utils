package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/config"
)

func archiveRepo(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	t.Setenv(routesEnv, "")
	cfg := config.Defaults()
	cfg.SyncDir = filepath.Join(home, "sync")
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	for rel, body := range map[string]string{
		"archive/2026-10-20/p1/old.md": "alpha\nneedle one\n",
		"archive/2026-10-25/p1/new.md": "beta\n",
	} {
		p := filepath.Join(cfg.SyncDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return cfg.Path()
}

func runArch(t *testing.T, args ...string) (string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := archiveCmd(&out, &errb, args)
	if code != 0 {
		t.Logf("stderr: %s", errb.String())
	}
	return out.String(), code
}

func TestArchiveList_Since(t *testing.T) {
	cfg := archiveRepo(t)
	out, code := runArch(t, "list", "--config", cfg)
	if code != 0 || !strings.Contains(out, "old.md") || !strings.Contains(out, "new.md") {
		t.Fatalf("code %d:\n%s", code, out)
	}
	out, _ = runArch(t, "list", "--since", "2026-10-22", "--config", cfg)
	if strings.Contains(out, "old.md") || !strings.Contains(out, "new.md") {
		t.Errorf("--since:\n%s", out)
	}
}

func TestArchiveSearch_PathsAndCountsByDefault_ShowPrintsLines(t *testing.T) {
	cfg := archiveRepo(t)
	out, code := runArch(t, "search", "needle", "--config", cfg)
	if code != 0 || !strings.Contains(out, "archive/2026-10-20/p1/old.md") || !strings.Contains(out, "1 matching line") {
		t.Fatalf("code %d:\n%s", code, out)
	}
	if strings.Contains(out, "needle one") {
		t.Errorf("default output must not print file contents:\n%s", out)
	}
	out, _ = runArch(t, "search", "needle", "--show", "--config", cfg)
	if !strings.Contains(out, "needle one") {
		t.Errorf("--show:\n%s", out)
	}
}

func TestArchive_Usage(t *testing.T) {
	if _, code := runArch(t); code != 2 {
		t.Errorf("no args: %d", code)
	}
	if _, code := runArch(t, "search"); code != 2 {
		t.Errorf("search without a term: %d", code)
	}
	if _, code := runArch(t, "purge"); code != 2 {
		t.Errorf("there is no purge command: %d", code)
	}
}

func TestInit_ArchiveDeletesFlagSetsConfig(t *testing.T) {
	cfg, _ := initFixture(t)
	cfg.ArchiveDeletes = true
	if err := bootstrap(cfg, false, nil); err != nil {
		t.Fatal(err)
	}
	got, err := config.Load(cfg.Path())
	if err != nil || !got.ArchiveDeletes {
		t.Fatalf("archiveDeletes not saved: %+v %v", got.ArchiveDeletes, err)
	}
	// default stays off
	if config.Defaults().ArchiveDeletes {
		t.Error("archiveDeletes must default to false")
	}
}
