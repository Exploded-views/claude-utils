//go:build !windows

package sync

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
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
	if strings.Contains(first[0], "gc.auto=0") || strings.Contains(first[0], "maintenance.auto") {
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

// Only the daemon's loop runs housekeeping: nothing else in the product (the
// changes hook above all) calls Repo.Maintain.
func TestMaintainIsOnlyCalledByTheLoop(t *testing.T) {
	var bad []string
	filepath.WalkDir("../..", func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), ".Maintain(") && filepath.ToSlash(p) != "../../internal/sync/loop.go" {
			bad = append(bad, p)
		}
		return nil
	})
	if len(bad) > 0 {
		t.Errorf("Repo.Maintain called outside the daemon loop: %v", bad)
	}
	if b, _ := os.ReadFile("loop.go"); !strings.Contains(string(b), ".Maintain(") {
		t.Error("the loop never calls Repo.Maintain")
	}
}
