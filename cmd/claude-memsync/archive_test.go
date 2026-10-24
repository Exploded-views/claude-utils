package main

import (
	"bytes"
	"os"
	"os/exec"
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

func TestArchiveSearch_HidesDenylistedPathsAndCountsThem(t *testing.T) {
	cfgPath := archiveRepo(t)
	cfg, _ := config.Load(cfgPath)
	if err := os.WriteFile(cfg.DenylistPath(), []byte("old\\.md\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"search", "needle", "--show"}, {"list"}} {
		out, code := runArch(t, append(args, "--config", cfgPath)...)
		if code != 0 || strings.Contains(out, "old.md") || strings.Contains(out, "needle one") {
			t.Errorf("%v leaked a denylisted name:\n%s", args, out)
		}
		if !strings.Contains(out, "1 hidden by the denylist") {
			t.Errorf("%v: hidden count missing:\n%s", args, out)
		}
	}
}

func TestArchiveSearch_NeverReadsThroughASymlink(t *testing.T) {
	cfgPath := archiveRepo(t)
	cfg, _ := config.Load(cfgPath)
	outside := filepath.Join(t.TempDir(), "outside.txt")
	_ = os.WriteFile(outside, []byte("needle outside\n"), 0o600)
	if err := os.Symlink(outside, filepath.Join(cfg.SyncDir, "archive", "2026-10-25", "p1", "l.md")); err != nil {
		t.Skip("no symlinks here")
	}
	out, _ := runArch(t, "search", "outside", "--show", "--config", cfgPath)
	if strings.Contains(out, "needle outside") || strings.Contains(out, "l.md") {
		t.Errorf("read through a symlink:\n%s", out)
	}
}

// init keeps the lines a clone already has and adds the index's union rule, so
// two hosts appending to archive/INDEX.md merge without a conflict.
func TestInit_ArchiveIndexMergesAcrossHosts(t *testing.T) {
	cfg, origin := initFixture(t)
	cfg.ArchiveDeletes = true
	// A peer already pushed a .gitattributes with a line of its own.
	seed := t.TempDir()
	run := func(dir string, args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return string(out)
	}
	run(seed, "init", "-q", "-b", "main")
	_ = os.WriteFile(filepath.Join(seed, ".gitattributes"), []byte("*.png binary\n"), 0o600)
	run(seed, "add", "-A")
	run(seed, "commit", "-q", "-m", "seed")
	run(seed, "push", "-q", origin, "main")

	if err := bootstrap(cfg, false, nil); err != nil {
		t.Fatal(err)
	}
	attrs, _ := os.ReadFile(filepath.Join(cfg.SyncDir, ".gitattributes"))
	for _, w := range []string{"*.png binary", "archive/INDEX.md merge=union", "MEMORY.md merge="} {
		if !strings.Contains(string(attrs), w) {
			t.Errorf(".gitattributes lacks %q:\n%s", w, attrs)
		}
	}

	hostB := t.TempDir()
	run(hostB, "clone", "-q", origin, "b")
	b := filepath.Join(hostB, "b")
	appendIndex := func(dir, line string) {
		t.Helper()
		f, err := os.OpenFile(filepath.Join(dir, "archive", "INDEX.md"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			_ = os.MkdirAll(filepath.Join(dir, "archive"), 0o700)
			f, err = os.OpenFile(filepath.Join(dir, "archive", "INDEX.md"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatal(err)
			}
		}
		_, _ = f.WriteString(line + "\n")
		f.Close()
		run(dir, "add", "-A")
		run(dir, "commit", "-q", "-m", line)
	}
	appendIndex(cfg.SyncDir, "- from host A")
	appendIndex(b, "- from host B")
	run(cfg.SyncDir, "push", "-q", "origin", "main")
	run(b, "pull", "--rebase", "origin", "main") // fails on a conflict
	idx, _ := os.ReadFile(filepath.Join(b, "archive", "INDEX.md"))
	if !strings.Contains(string(idx), "host A") || !strings.Contains(string(idx), "host B") {
		t.Errorf("INDEX.md after the merge:\n%s", idx)
	}
}
