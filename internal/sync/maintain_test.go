//go:build !windows

package sync

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/proc"
)

// fakeGit puts a `git` first on PATH that appends its argv to log and then runs body.
func fakeGit(t *testing.T, body string) (log string) {
	t.Helper()
	bin := t.TempDir()
	log = filepath.Join(bin, "calls")
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func calls(t *testing.T, log string) []string {
	t.Helper()
	b, _ := os.ReadFile(log)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func maintLoop(t *testing.T, now *time.Time) (*Loop, *gitwt.Repo) {
	t.Helper()
	dir := t.TempDir()
	l := &Loop{Cfg: config.Config{SyncDir: dir}, Now: func() time.Time { return *now }}
	return l, gitwt.New(dir)
}

// Housekeeping runs once per 24 hours of the injected clock, in the
// foreground, and without the flag that turns auto gc off.
func TestMaintainRunsOncePer24Hours(t *testing.T) {
	log := fakeGit(t, "exit 0")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l, repo := maintLoop(t, &now)
	n := func() int {
		b, _ := os.ReadFile(log)
		return len(strings.Fields(strings.ReplaceAll(string(b), "\n", " "))) // any output at all
	}
	l.maybeMaintain(repo)
	if n() == 0 {
		t.Fatal("first tick: maintenance did not run")
	}
	first := calls(t, log)
	if len(first) != 1 || !strings.Contains(first[0], "gc") || !strings.Contains(first[0], "--auto") {
		t.Fatalf("calls = %q, want one gc --auto", first)
	}
	if strings.Contains(first[0], "gc.auto=") || strings.Contains(first[0], "maintenance.auto=") {
		t.Errorf("housekeeping carries the flag that disables it: %q", first[0])
	}
	if !strings.Contains(first[0], "gc.autoDetach=false") {
		t.Errorf("housekeeping may detach: %q", first[0])
	}
	now = now.Add(23*time.Hour + 59*time.Minute)
	l.maybeMaintain(repo)
	if got := len(calls(t, log)); got != 1 {
		t.Errorf("ran again after 23h59m: %d calls", got)
	}
	now = now.Add(time.Minute)
	l.maybeMaintain(repo)
	if got := len(calls(t, log)); got != 2 {
		t.Errorf("did not run at 24h: %d calls", got)
	}
}

// A run that outlives MaintainTimeout is killed with its whole process group.
func TestMaintainHonoursTheTimeout(t *testing.T) {
	pids := filepath.Join(t.TempDir(), "pid")
	fakeGit(t, "echo $$ > "+pids+"\nexec sleep 60")
	now := time.Now()
	l, repo := maintLoop(t, &now)
	l.MaintainTimeout = 300 * time.Millisecond
	t.Cleanup(func() {
		if b, _ := os.ReadFile(pids); len(b) > 0 {
			if p, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				_ = syscall.Kill(p, syscall.SIGKILL)
			}
		}
	})
	done := make(chan struct{})
	go func() { defer close(done); l.maybeMaintain(repo) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("maintenance still running long after its timeout")
	}
	time.Sleep(200 * time.Millisecond)
	b, _ := os.ReadFile(pids)
	p, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("the fake git never ran: %q", b)
	}
	if syscall.Kill(p, 0) == nil {
		t.Errorf("git %d still running after the timeout", p)
	}
}

// A held loop (foreign writer, expired host) runs no housekeeping.
func TestMaintainSkipsWhileHeld(t *testing.T) {
	log := fakeGit(t, "exit 0")
	now := time.Now()
	l, repo := maintLoop(t, &now)
	l.Hold = func() bool { return true }
	l.maybeMaintain(repo)
	if _, err := os.Stat(log); err == nil {
		t.Error("housekeeping ran while the loop was held")
	}
}

// spawnsOf counts the children proc starts whose argv contains word, until the
// returned func is called.
func countSpawns(word string) (n *atomic.Int64, stop func()) {
	n = new(atomic.Int64)
	restore := proc.SetSpawnHook(func(_ string, args []string) {
		for _, a := range args {
			if a == word {
				n.Add(1)
				return
			}
		}
	})
	return n, restore
}

// Cancelling the loop's ctx during a hanging gc returns at once and takes the
// gc's whole process group with it, though MaintainTimeout is long.
func TestMaintainStopsWithTheLoopContext(t *testing.T) {
	gpid := filepath.Join(t.TempDir(), "gpid")
	fakeGit(t, "sleep 60 &\necho $! > "+gpid+"\nwait")
	now := time.Now()
	l, repo := maintLoop(t, &now)
	l.MaintainTimeout = 10 * time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	l.ctx = ctx
	t.Cleanup(func() { killPidFile(gpid) })
	done := make(chan struct{})
	go func() { defer close(done); l.maybeMaintain(repo) }()
	waitFile(t, gpid)
	start := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("housekeeping still running 2s after the loop's context was cancelled")
	}
	t.Logf("returned %v after cancel", time.Since(start))
	assertDead(t, gpid)
}

// The timeout kills the gc's children too (a real gc forks repack and
// pack-objects), not just git itself.
func TestMaintainTimeoutKillsTheProcessGroup(t *testing.T) {
	gpid := filepath.Join(t.TempDir(), "gpid")
	fakeGit(t, "sleep 60 &\necho $! > "+gpid+"\nwait")
	now := time.Now()
	l, repo := maintLoop(t, &now)
	l.MaintainTimeout = 300 * time.Millisecond
	t.Cleanup(func() { killPidFile(gpid) })
	l.maybeMaintain(repo)
	assertDead(t, gpid)
}

func waitFile(t *testing.T, p string) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if b, _ := os.ReadFile(p); strings.TrimSpace(string(b)) != "" {
			return
		}
	}
	t.Fatalf("%s never written", p)
}

func killPidFile(p string) {
	if b, _ := os.ReadFile(p); len(b) > 0 {
		if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			_ = syscall.Kill(n, syscall.SIGKILL)
		}
	}
}

func assertDead(t *testing.T, pidFile string) {
	t.Helper()
	b, _ := os.ReadFile(pidFile)
	p, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("the fake gc never started its child: %q", b)
	}
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if syscall.Kill(p, 0) != nil {
			return
		}
	}
	t.Errorf("child %d of the gc is still running", p)
}

// A pull tick past 24 hours of the injected clock really runs gc (seen through
// proc's spawn seam), and one inside the period does not.
func TestPullTickRunsHousekeepingWhenDue(t *testing.T) {
	f := newPullFixture(t, map[string]string{})
	captureLog(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f.loop.Now = func() time.Time { return now }
	n, stop := countSpawns("gc")
	defer stop()
	f.loop.pullTick(f.repo, f.roots)
	if got := n.Load(); got != 1 {
		t.Fatalf("first tick: %d gc runs, want 1", got)
	}
	now = now.Add(time.Hour)
	f.loop.pullTick(f.repo, f.roots)
	if got := n.Load(); got != 1 {
		t.Errorf("tick 1h later: %d gc runs, want 1", got)
	}
	now = now.Add(25 * time.Hour)
	f.loop.pullTick(f.repo, f.roots)
	if got := n.Load(); got != 2 {
		t.Errorf("tick 26h later: %d gc runs, want 2", got)
	}
}

// At most one housekeeping runs at a time across all loops of the process.
func TestMaintainIsSerialisedAcrossLoops(t *testing.T) {
	dir := t.TempDir()
	live, max := filepath.Join(dir, "live"), filepath.Join(dir, "max")
	if err := os.Mkdir(live, 0o700); err != nil {
		t.Fatal(err)
	}
	// Each live gc is a directory named by its pid; max is the highest count seen.
	fakeGit(t, `mkdir `+live+`/$$
n=$(ls `+live+` | wc -l | tr -d ' ')
echo $n >> `+max+`
sleep 0.3
rmdir `+live+`/$$`)
	now := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		l, repo := maintLoop(t, &now)
		wg.Add(1)
		go func() { defer wg.Done(); l.maybeMaintain(repo) }()
	}
	wg.Wait()
	b, _ := os.ReadFile(max)
	for _, n := range strings.Fields(string(b)) {
		if n != "1" {
			t.Fatalf("concurrent gcs seen: %q, want only 1", strings.Fields(string(b)))
		}
	}
	if len(strings.Fields(string(b))) != 3 {
		t.Errorf("gcs run = %d, want 3", len(strings.Fields(string(b))))
	}
}

// A restart does not redo housekeeping done within the period: the last run
// is kept in the sync dir's .state.
func TestMaintainRemembersTheLastRunAcrossRestarts(t *testing.T) {
	log := fakeGit(t, "exit 0")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l, repo := maintLoop(t, &now)
	l.maybeMaintain(repo)
	restarted := &Loop{Cfg: l.Cfg, Now: l.Now} // a new process: no in-memory state
	now = now.Add(2 * time.Hour)
	restarted.maybeMaintain(repo)
	if got := len(calls(t, log)); got != 1 {
		t.Errorf("a restart 2h later ran housekeeping again: %d runs", got)
	}
	if _, err := os.Stat(filepath.Join(l.Cfg.SyncDir, ".state", "maintained")); err != nil {
		t.Errorf("no record of the run: %v", err)
	}
	now = now.Add(23 * time.Hour)
	restarted.maybeMaintain(repo)
	if got := len(calls(t, log)); got != 2 {
		t.Errorf("a restart 25h after the run did not run it: %d runs", got)
	}
}
