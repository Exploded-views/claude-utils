//go:build budget

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	syncpkg "github.com/MarimerLLC/claude-utils/internal/sync"
	"github.com/MarimerLLC/claude-utils/internal/testenv"
)

// procStat is the CPU time (user+system) and resident set of a process.
func procStat(t *testing.T, pid int) (cpu time.Duration, rssKB int64) {
	t.Helper()
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		s := string(b)
		f := strings.Fields(s[strings.LastIndex(s, ")")+2:]) // from field 3 on
		ut, _ := strconv.ParseInt(f[11], 10, 64)
		st, _ := strconv.ParseInt(f[12], 10, 64)
		cpu = time.Duration(ut+st) * 10 * time.Millisecond // USER_HZ is 100
		if st, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid)); err == nil {
			for _, l := range strings.Split(string(st), "\n") {
				if strings.HasPrefix(l, "VmRSS:") {
					rssKB, _ = strconv.ParseInt(strings.Fields(l)[1], 10, 64)
				}
			}
		}
		return cpu, rssKB
	}
	out, err := exec.Command("ps", "-o", "time=,rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	f := strings.Fields(string(out))
	if len(f) != 2 {
		t.Fatalf("ps output %q", out)
	}
	// [[HH:]MM:]SS[.cc]
	secs := 0.0
	for _, p := range strings.Split(f[0], ":") {
		v, _ := strconv.ParseFloat(p, 64)
		secs = secs*60 + v
	}
	rssKB, _ = strconv.ParseInt(f[1], 10, 64)
	return time.Duration(secs * float64(time.Second)), rssKB
}

// B2 and B3: after its start-up pass, the daemon idles under 1% of one core
// (averaged over 60 s) and holds under 64 MB. The start-up pass itself ends
// within 10 s. Fixture: the budget tree and five repos with local bare remotes.
func TestBudgetB2B3_DaemonIdle(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_SYSTEM": os.DevNull,
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.com",
		"GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.com",
		"CLAUDE_PROJECT_DIR": "", "XDG_CONFIG_HOME": "",
	} {
		t.Setenv(k, v)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	ms, mm := testenv.Binaries(t)
	tree := newBudgetTree(t, t.TempDir(), 10000)

	claude := filepath.Join(home, ".claude", "projects")
	for _, group := range [][]string{tree.missing, tree.real, tree.other} {
		for _, f := range group {
			mem := filepath.Join(claude, f, "memory")
			if err := os.MkdirAll(mem, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(mem, "n.md"), []byte("note\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	entries := map[string]any{}
	var cfgs []config.Config
	for _, name := range budgetRepos {
		root := t.TempDir()
		origin := filepath.Join(root, "origin.git")
		if out, err := exec.Command("git", "init", "-q", "--bare", "-b", "main", origin).CombinedOutput(); err != nil {
			t.Fatalf("init bare: %v: %s", err, out)
		}
		cfg := config.Defaults()
		cfg.SyncDir = filepath.Join(home, ".claudesync.d", name)
		cfg.ClaudeProjectsDir = claude
		cfg.RemoteURL = origin
		cfg.MergeDriverPath = mm
		if err := bootstrapWith(cfg, false, nil, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := gitwt.New(cfg.SyncDir).Run("rev-parse", "HEAD"); err != nil {
			t.Fatal(err)
		}
		if err := config.Save(cfg); err != nil {
			t.Fatal(err)
		}
		// Bootstrap leaves a manifest; the daemon's first save marks its start-up pass.
		_ = os.Remove(syncpkg.ManifestPath(cfg.SyncDir))
		kind := "project"
		if name == "personal" {
			kind = "personal"
		}
		entries[name] = map[string]any{"config": cfg.Path(), "kind": kind}
		cfgs = append(cfgs, cfg)
	}
	rj, _ := json.Marshal(map[string]any{"repos": entries, "routes": budgetRoutes(), "default": "deny", "personal": "personal"})
	routesPath := filepath.Join(home, ".config", "claude-memsync", "routes.json")
	if err := os.MkdirAll(filepath.Dir(routesPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(routesPath, rj, 0o600); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	cmd := exec.Command(ms, "run", "--routes", routesPath, "--skip-quiet-window")
	cmd.Env = append(os.Environ(), "TMPDIR="+tree.tmp, "CLAUDE_MEMSYNC_ROUTES=")
	cmd.Stderr, cmd.Stdout = &stderr, &stderr
	started := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer func() { // stop it by its exact PID
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
	}()

	// The start-up pass is over when every loop has saved its manifest.
	deadline := time.Now().Add(120 * time.Second)
	var startup time.Duration
	for {
		done := 0
		for _, c := range cfgs {
			if _, err := os.Stat(syncpkg.ManifestPath(c.SyncDir)); err == nil {
				done++
			}
		}
		if done == len(cfgs) {
			startup = time.Since(started)
			break
		}
		select {
		case err := <-exited:
			t.Fatalf("daemon exited early: %v\n%s", err, stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("B-startup: %d of %d manifests after 120s\n%s", done, len(cfgs), tail(stderr.String()))
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("start-up pass ended after %v", startup)
	if startup >= 10*time.Second {
		t.Errorf("B-startup: start-up pass took %v, budget 10s", startup)
	}

	time.Sleep(3 * time.Second) // let the start-up settle
	cpu0, _ := procStat(t, pid)
	time.Sleep(60 * time.Second)
	cpu1, rss := procStat(t, pid)
	pct := float64(cpu1-cpu0) / float64(60*time.Second) * 100
	t.Logf("B2 idle CPU %.2f%% of one core over 60s (%v cpu); B3 RSS %.1f MB (%s/%s)", pct, cpu1-cpu0, float64(rss)/1024, runtime.GOOS, runtime.GOARCH)
	if pct >= 1 {
		t.Errorf("B2: idle CPU %.2f%%, budget 1%%", pct)
	}
	if rss >= 64*1024 {
		t.Errorf("B3: RSS %.1f MB, budget 64 MB", float64(rss)/1024)
	}
}

func tail(s string) string {
	if len(s) > 2000 {
		return s[len(s)-2000:]
	}
	return s
}
