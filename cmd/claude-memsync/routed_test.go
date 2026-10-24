package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/lock"
)

// routedHost builds a fake host: a fake HOME, the named sync repos (each an
// initialised git repo with its own config.json), and a routes.json. It sets
// CLAUDE_MEMSYNC_ROUTES and returns the config of each repo.
func routedHost(t *testing.T, repos []string, personal string, routes []map[string]string, extraRepos ...string) map[string]config.Config {
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
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("CLAUDE_PROJECT_DIR", "")

	claude := filepath.Join(home, ".claude", "projects")
	cfgs := map[string]config.Config{}
	entries := map[string]any{}
	for _, name := range repos {
		cfg := config.Defaults()
		cfg.SyncDir = filepath.Join(home, ".claudesync.d", name)
		cfg.ClaudeProjectsDir = claude
		repo := gitwt.New(cfg.SyncDir)
		if err := repo.Init("main"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfg.SyncDir, "README"), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
		_ = repo.AddAll()
		if _, err := repo.Commit("init"); err != nil {
			t.Fatal(err)
		}
		if err := config.Save(cfg); err != nil {
			t.Fatal(err)
		}
		cfgs[name] = cfg
		entries[name] = cfg.Path()
	}
	for _, name := range extraRepos { // named in routes.json, absent on this host
		entries[name] = map[string]any{"config": filepath.Join(home, ".claudesync.d", name, "config.json"), "client": true}
	}
	rj := map[string]any{"repos": entries, "routes": routes, "default": "deny"}
	if personal != "" {
		rj["personal"] = personal
	}
	b, _ := json.Marshal(rj)
	p := filepath.Join(home, ".config", "claude-memsync", "routes.json")
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(routesEnv, p)
	return cfgs
}

func TestRouted_LegacyWithoutRoutesFileIsUnchanged(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv(routesEnv, "")
	if _, ok, err := resolveRoutesPath(""); ok || err != nil {
		t.Fatalf("ok=%v err=%v: no routes file means single-repo mode", ok, err)
	}
	cfg, err := loadDistillConfig("")
	if err != nil || cfg.SyncDir != filepath.Join(home, ".claudesync") {
		t.Fatalf("legacy default config changed: %+v %v", cfg, err)
	}
	if got := pidFilePath(); got != filepath.Join(home, ".claudesync", "daemon.pid") {
		t.Fatalf("legacy pid path = %s", got)
	}
}

func TestRouted_ExplicitMissingRoutesIsAnError(t *testing.T) {
	t.Setenv(routesEnv, "")
	if _, _, err := resolveRoutesPath(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("a routes path that does not exist must not fall back to single-repo mode")
	}
}

func TestRouted_SecondDaemonOnSameSyncDirRefused(t *testing.T) {
	cfgs := routedHost(t, []string{"personal", "projects"}, "personal", []map[string]string{
		{"folder": "-a", "repo": "projects"},
	})
	set, err := loadRouted(os.Getenv(routesEnv))
	if err != nil {
		t.Fatal(err)
	}
	first, err := set.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// A second routed daemon, and a single-repo daemon on one of the dirs.
	if _, err := set.Start(context.Background()); !errors.Is(err, lock.ErrHeld) {
		t.Fatalf("second routed daemon: %v, want ErrHeld", err)
	}
	if _, err := startSingle(context.Background(), cfgs["projects"]); !errors.Is(err, lock.ErrHeld) {
		t.Fatalf("single daemon on a routed sync dir: %v, want ErrHeld", err)
	}
	_ = first.Stop()
	again, err := startSingle(context.Background(), cfgs["projects"])
	if err != nil {
		t.Fatalf("after stop: %v", err)
	}
	_ = again.Stop()
}

func TestRouted_ReposSharingASyncDirRejected(t *testing.T) {
	cfgs := routedHost(t, []string{"a", "b"}, "", nil)
	b := cfgs["b"]
	b.SyncDir = cfgs["a"].SyncDir
	// b's config.json is rewritten to point at a's sync dir.
	data, _ := json.Marshal(b)
	if err := os.WriteFile(filepath.Join(cfgs["b"].SyncDir, "config.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRouted(os.Getenv(routesEnv)); err == nil || !strings.Contains(err.Error(), "share the sync dir") {
		t.Fatalf("err = %v", err)
	}
}

func TestRouted_StatusListsReposOwnedDeniedShadowedMissing(t *testing.T) {
	cfgs := routedHost(t, []string{"personal", "projects"}, "personal", []map[string]string{
		{"folder": "-a", "repo": "projects"},
		{"folder": "-a", "repo": "projects"},         // shadowed
		{"folder": "-clientdir", "repo": "client-x"}, // repo absent
	}, "client-x")
	for _, f := range []string{"-a", "-clientdir", "-zzz"} {
		if err := os.MkdirAll(filepath.Join(cfgs["projects"].ClaudeProjectsDir, f, "memory"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	set, err := loadRouted(os.Getenv(routesEnv))
	if err != nil {
		t.Fatal(err)
	}
	out := strings.Join(set.statusLines(), "\n")
	for _, want := range []string{
		"projects: owns 1 folder",
		"personal: personal, owns no folders",
		"client-x: not on this host",
		"denied: 2 folders",
		"shadowed: rule 1 is never reached first (rule 0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
}

func TestRouted_DistillDefaultsToPersonalRepo(t *testing.T) {
	cfgs := routedHost(t, []string{"projects", "personal"}, "personal", nil)
	cfg, err := loadDistillConfig("")
	if err != nil || cfg.SyncDir != cfgs["personal"].SyncDir {
		t.Fatalf("got %s (%v), want the personal repo", cfg.SyncDir, err)
	}
}

// changes --hook reports, for the project dir, the notes of every routed repo,
// and a repo reports only the folders it owns.
func TestRouted_HookReportsAcrossReposOwnedFoldersOnly(t *testing.T) {
	cfgs := routedHost(t, []string{"personal", "projects", "client-a"}, "personal", []map[string]string{
		{"folder": "-home-alice-src-app", "repo": "projects"},
		{"folder": "-home-alice-src-cl", "repo": "client-a"},
	})
	app := hookJSON("sa", "/home/alice/src/app", "UserPromptSubmit")
	cl := hookJSON("sc", "/home/alice/src/cl", "UserPromptSubmit")
	runHook(t, app, "--hook")
	runHook(t, cl, "--hook") // baselines in every repo

	addNotes(t, cfgs["projects"].Path(), map[string]string{
		"-home-alice-src-app/memory/app-note.md": "a",
		"-home-alice-src-cl/memory/frozen.md":    "f", // a folder this repo does not own
	})
	addNotes(t, cfgs["client-a"].Path(), map[string]string{
		"-home-alice-src-cl/memory/cl-note.md":  "c",
		"-home-alice-src-app/memory/foreign.md": "x",
	})

	code, out, errs := runHook(t, app, "--hook")
	if code != 0 || errs != "" {
		t.Fatalf("exit %d stderr %q", code, errs)
	}
	if !strings.Contains(out, "app-note.md") || strings.Contains(out, "foreign.md") || strings.Contains(out, "cl-note.md") {
		t.Errorf("app project: %s", out)
	}
	_, out, _ = runHook(t, cl, "--hook")
	if !strings.Contains(out, "cl-note.md") || strings.Contains(out, "frozen.md") {
		t.Errorf("client project: %s", out)
	}
	if strings.Contains(out, "unowned") {
		t.Errorf("owned-folder filtering must not show up as hidden files: %s", out)
	}
}
