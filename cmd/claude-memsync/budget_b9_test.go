//go:build budget

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"bytes"
	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	syncpkg "github.com/MarimerLLC/claude-utils/internal/sync"
	"github.com/MarimerLLC/claude-utils/internal/testenv"
)

// B9: the daemon's start-up pass on the B2/B3 fixture, counted. The daemon is
// built with the budgetprobe tag (every proc.Run child is recorded through the
// proc spawn seam, with the peak number running at once) and a POSIX sh git
// shim on PATH counts calls, to cross-check the seam. Route resolution is told apart
// from the five sync loops by the directory git is run in.
func TestBudgetB9_StartupSpawns(t *testing.T) {
	logLoad(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	realGit, _ := exec.LookPath("git")
	// The build below runs with HOME in the temp dir; keep Go's caches where they are.
	goEnv, err := exec.Command("go", "env", "GOMODCACHE", "GOCACHE").Output()
	if err != nil {
		t.Fatal(err)
	}
	goDirs := strings.Fields(string(goEnv))
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
	_, mm := testenv.Binaries(t)
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

	// probe binary
	scratch := t.TempDir()
	probeBin := filepath.Join(scratch, "claude-memsync-probe")
	root, _ := filepath.Abs("../..")
	bc := exec.Command("go", "build", "-tags", "budgetprobe", "-o", probeBin, "./cmd/claude-memsync")
	bc.Dir = root
	if len(goDirs) == 2 {
		bc.Env = append(os.Environ(), "GOMODCACHE="+goDirs[0], "GOCACHE="+goDirs[1])
	}
	if out, err := bc.CombinedOutput(); err != nil {
		t.Fatalf("build probe: %v\n%s", err, out)
	}
	// git shim, installed after the fixture is built so setup is not logged
	shimDir := t.TempDir()
	shimLog := filepath.Join(shimDir, "log")
	shim := "#!/bin/sh\necho x \"$@\" >> " + shimLog + "\nexec " + realGit + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	probeOut := filepath.Join(scratch, "probe.json")
	var stderr bytes.Buffer
	cmd := exec.Command(probeBin, "run", "--routes", routesPath, "--skip-quiet-window")
	cmd.Env = append(os.Environ(), "TMPDIR="+tree.tmp, "CLAUDE_MEMSYNC_ROUTES=", "MEMSYNC_PROBE_OUT="+probeOut)
	cmd.Stderr, cmd.Stdout = &stderr, &stderr
	started := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
	}()
	doneAt := map[string]time.Duration{}
	deadline := time.Now().Add(150 * time.Second)
	for len(doneAt) < len(cfgs) {
		for i, c := range cfgs {
			if _, err := os.Stat(syncpkg.ManifestPath(c.SyncDir)); err == nil {
				if _, ok := doneAt[budgetRepos[i]]; !ok {
					doneAt[budgetRepos[i]] = time.Since(started)
				}
			}
		}
		select {
		case err := <-exited:
			t.Fatalf("daemon exited early: %v\n%s", err, stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d manifests\n%s", len(doneAt), tail(stderr.String()))
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = cmd.Process.Signal(syscall.SIGUSR1)
	var probe struct {
		Spawns  [][]string `json:"spawns"`
		PeakAll int        `json:"peak_all"`
		PeakRes int        `json:"peak_res"`
	}
	for i := 0; i < 100; i++ {
		if b, err := os.ReadFile(probeOut); err == nil && json.Unmarshal(b, &probe) == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Classify each spawn by the directory it ran in: the sync repos live under
	// home/.claudesync.d, a real checkout is tree.tmp/checkoutNNN, a non-repo
	// is root/other/oNNN.
	syncRoot := filepath.Join(home, ".claudesync.d")
	var total, loops, perCheckout, perOther, otherRes int
	perDir := map[string]int{}
	spawns := probe.Spawns
	for _, a := range spawns {
		total++
		dir := ""
		for i := 1; i+1 < len(a); i++ {
			if a[i] == "-C" {
				dir = a[i+1]
				break
			}
		}
		switch {
		case strings.HasPrefix(dir, syncRoot):
			loops++
		case strings.Contains(dir, string(filepath.Separator)+"other"+string(filepath.Separator)+"o"):
			perOther++
			perDir[dir]++
		case strings.Contains(dir, string(filepath.Separator)+"checkout"):
			perCheckout++
			perDir[dir]++
		default:
			otherRes++
		}
	}
	maxPerDir := 0
	for _, n := range perDir {
		if n > maxPerDir {
			maxPerDir = n
		}
	}
	resolution := perCheckout + perOther
	// Peak children running at once come from the daemon's spawn seam; the
	// shim only counts calls, to cross-check the seam.
	sb, _ := os.ReadFile(shimLog)
	shimmed := strings.Count(string(sb), "x ")
	peakRes, peakAll := probe.PeakRes, probe.PeakAll
	t.Logf("COUNTS total=%d loops=%d resolution=%d (checkouts %d over %d dirs, max %d per dir; non-repo %d; no -C (loops) %d) peak resolution children=%d peak all git children=%d shim calls=%d",
		total, loops, resolution, perCheckout, len(perDir)-perOther, maxPerDir, perOther, otherRes, peakRes, peakAll, shimmed)

	nCheckouts := len(tree.real)
	if perCheckout > nCheckouts || maxPerDir > 1 {
		t.Errorf("resolution spawns per real checkout: %d in total (limit %d, one per checkout), up to %d for one dir", perCheckout, nCheckouts, maxPerDir)
	}
	// A direct "no" is never taken on its own: git confirms each existing
	// non-repo once (a gone directory costs nothing).
	if perOther != len(tree.other) {
		t.Errorf("non-repo dirs cost %d git spawns, want exactly %d (one each)", perOther, len(tree.other))
	}
	if limit := 300 + 100 + 5*59; total > limit {
		t.Errorf("start-up spawned %d git children, limit %d", total, limit)
	}
	if shimmed != total {
		t.Errorf("the git shim saw %d calls, the spawn seam %d", shimmed, total)
	}
	if peakRes > 4 {
		t.Errorf("peak concurrent resolution git children %d, limit 4", peakRes)
	}
}
