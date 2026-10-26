package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/changes"
	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/testenv"
)

// BenchmarkChangesHook times one `changes --hook` run over three age-encrypted
// repos of about 50 notes each, with a pending change in the one that owns the
// hook's project. It reports ns/op only; there is no budget yet.
func BenchmarkChangesHook(b *testing.B) {
	if _, err := exec.LookPath("git"); err != nil {
		b.Skip("git not on PATH")
	}
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_SYSTEM": os.DevNull,
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.com",
		"GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.com",
		"CLAUDE_PROJECT_DIR": "", "XDG_CONFIG_HOME": "",
	} {
		b.Setenv(k, v)
	}
	home := b.TempDir()
	b.Setenv("HOME", home)
	b.Setenv("USERPROFILE", home)
	ms, mm := testenv.Binaries(b)

	const session = "bench"
	entries := map[string]any{}
	var routes []map[string]string
	type repoState struct {
		cfg    config.Config
		marker string
		base   string
	}
	var states []repoState
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("repo%d", i)
		folder := fmt.Sprintf("-bench-%d", i)
		if i == 0 {
			folder = "-home-alice-src-app" // the hook's project: this repo has the news
		}
		root := b.TempDir()
		origin := filepath.Join(root, "origin.git")
		if out, err := exec.Command("git", "init", "-q", "--bare", "-b", "main", origin).CombinedOutput(); err != nil {
			b.Fatalf("init bare: %v: %s", err, out)
		}
		cfg := config.Defaults()
		cfg.SyncDir = filepath.Join(home, ".claudesync.d", name)
		cfg.ClaudeProjectsDir = filepath.Join(root, "claude", "projects")
		cfg.RemoteURL = origin
		cfg.MergeDriverPath = mm
		mem := filepath.Join(cfg.ClaudeProjectsDir, folder, "memory")
		if err := os.MkdirAll(mem, 0o700); err != nil {
			b.Fatal(err)
		}
		for n := 0; n < 50; n++ {
			body := fmt.Sprintf("note %d of %s\n%s\n", n, name, strings.Repeat("some memory text ", 20))
			if err := os.WriteFile(filepath.Join(mem, fmt.Sprintf("note_%02d.md", n)), []byte(body), 0o600); err != nil {
				b.Fatal(err)
			}
		}
		enc := &encryptSpec{IdentityPath: filepath.Join(root, "k", "identity.txt"), MemsyncExe: ms}
		if err := bootstrapWith(cfg, false, nil, enc); err != nil {
			b.Fatal(err)
		}
		repo := gitwt.New(cfg.SyncDir)
		base, err := repo.Run("rev-parse", "HEAD")
		if err != nil {
			b.Fatal(err)
		}
		p := filepath.Join(cfg.SyncDir, "projects", folder, "memory", "pending.md")
		if err := os.WriteFile(p, []byte("a note that arrived since the marker\n"), 0o600); err != nil {
			b.Fatal(err)
		}
		if err := repo.AddAll(); err != nil {
			b.Fatal(err)
		}
		if _, err := repo.Commit("auto: pending"); err != nil {
			b.Fatal(err)
		}
		// With no record of what the daemon copied, the hook reports up to HEAD.
		os.Remove(filepath.Join(cfg.SyncDir, ".state", "synced-head"))
		if err := config.Save(cfg); err != nil {
			b.Fatal(err)
		}
		entries[name] = map[string]any{"config": cfg.Path(), "kind": "project"}
		routes = append(routes, map[string]string{"folder": folder, "repo": name})
		states = append(states, repoState{cfg, filepath.Join(cfg.SyncDir, ".state", "hook-markers", session), strings.TrimSpace(base)})
	}
	rj, _ := json.Marshal(map[string]any{"repos": entries, "routes": routes, "default": "deny"})
	routesPath := filepath.Join(home, ".config", "claude-memsync", "routes.json")
	if err := os.MkdirAll(filepath.Dir(routesPath), 0o700); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(routesPath, rj, 0o600); err != nil {
		b.Fatal(err)
	}
	b.Setenv(routesEnv, routesPath)

	in := hookJSON(session, "/home/alice/src/app", "UserPromptSubmit")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		for _, s := range states { // rewind: every run has something to report
			if err := os.MkdirAll(filepath.Dir(s.marker), 0o700); err != nil {
				b.Fatal(err)
			}
			if err := changes.WriteMarker(s.marker, s.base); err != nil {
				b.Fatal(err)
			}
		}
		b.StartTimer()
		var out, errb bytes.Buffer
		if code := changesCmd(strings.NewReader(in), &out, &errb, []string{"--hook", "--deadline", "0"}); code != 0 || out.Len() == 0 {
			b.Fatalf("exit %d stdout %q stderr %q", code, out.String(), errb.String())
		}
	}
}
