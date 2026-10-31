package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

func fsIsCaseInsensitive(t *testing.T) bool {
	t.Helper()
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "a"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := os.Stat(filepath.Join(d, "A"))
	return err == nil
}

// trackedAsOtherCase makes the repo track "Config.json" at HEAD and in the
// index while the work-tree file stays config.json, with git's own case
// handling off so the pathspec alone cannot find it. On a case-insensitive file
// system both spellings are one file; elsewhere the tracked entry is a symlink
// to config.json, so it resolves to the same file.
func trackedAsOtherCase(t *testing.T, dir string) {
	t.Helper()
	repo := gitwt.New(dir)
	for _, a := range [][]string{{"config", "core.ignorecase", "false"}, {"config", "core.symlinks", "true"}} {
		if _, err := repo.Run(a...); err != nil {
			t.Fatal(err)
		}
	}
	if fsIsCaseInsensitive(t) {
		if err := os.Rename(filepath.Join(dir, "config.json"), filepath.Join(dir, "Config.json")); err != nil {
			t.Fatal(err)
		}
		defer os.Rename(filepath.Join(dir, "Config.json"), filepath.Join(dir, "config.json"))
	} else if err := os.Symlink("config.json", filepath.Join(dir, "Config.json")); err != nil {
		t.Skip("no symlinks:", err)
	}
	if _, err := repo.Run("add", "-f", "Config.json"); err != nil {
		t.Fatal(err)
	}
}

func TestConfigTrusted_RefusesFileTrackedUnderAnotherCase(t *testing.T) {
	for _, commit := range []bool{false, true} {
		cfg, _ := trustFixture(t)
		trackedAsOtherCase(t, cfg.SyncDir)
		if commit {
			if _, err := gitwt.New(cfg.SyncDir).Run("commit", "-q", "-m", "x"); err != nil {
				t.Fatal(err)
			}
			if _, err := gitwt.New(cfg.SyncDir).Run("rm", "-q", "--cached", "Config.json"); err != nil {
				t.Fatal(err)
			}
		}
		err := configTrusted(cfg)
		if err == nil || !strings.Contains(err.Error(), "tracked") {
			t.Errorf("committed=%v: a config.json that git tracks as Config.json must not be trusted, got %v", commit, err)
		}
	}
}

// A different file that merely has a case-variant name (possible on a
// case-sensitive file system only) is not the config file.
func TestConfigTrusted_AcceptsADifferentFileWithAnotherCase(t *testing.T) {
	if fsIsCaseInsensitive(t) {
		t.Skip("one file on a case-insensitive file system")
	}
	cfg, _ := trustFixture(t)
	if err := os.WriteFile(filepath.Join(cfg.SyncDir, "Config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := gitwt.New(cfg.SyncDir)
	repo.Run("config", "core.ignorecase", "false")
	if _, err := repo.Run("add", "-f", "Config.json"); err != nil {
		t.Fatal(err)
	}
	if err := configTrusted(cfg); err != nil {
		t.Errorf("a distinct, tracked Config.json does not make config.json untrusted: %v", err)
	}
}

// A tracked name that folds to the config's name but is another file (trailing
// space or dot: a different name except on Windows, where the same-file test
// then fires) does not make config.json untrusted on a file system that keeps
// them apart.
func TestConfigTrusted_AcceptsADifferentFileThatFoldsToTheName(t *testing.T) {
	cfg, _ := trustFixture(t)
	name := "config.json "
	if err := os.WriteFile(filepath.Join(cfg.SyncDir, name), []byte("{}"), 0o600); err != nil {
		t.Skip("file system does not keep this name:", err)
	}
	if st, err := os.Stat(filepath.Join(cfg.SyncDir, name)); err != nil || func() bool { a, _ := os.Stat(cfg.ConfigPath); return os.SameFile(a, st) }() {
		t.Skip("the name is the same file here")
	}
	if _, err := gitwt.New(cfg.SyncDir).Run("add", "-f", name); err != nil {
		t.Skip("git does not take this name here:", err)
	}
	if err := configTrusted(cfg); err != nil {
		t.Errorf("a distinct tracked file must not make config.json untrusted: %v", err)
	}
}
