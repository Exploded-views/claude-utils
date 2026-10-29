//go:build !windows

package related

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/proc"
)

// The git calls the changes hook makes on a related repo (Head, ChangedSince)
// go through proc: counted by its spawn seam, grouped, and killed by KillAll at
// the hook's deadline.
func TestHookGitOnARelatedRepoGoesThroughProc(t *testing.T) {
	bin := t.TempDir()
	pids := filepath.Join(bin, "pids")
	script := "#!/bin/sh\necho $$ >> " + pids + "\nexec sleep 60\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GIT_SSH_COMMAND", "")
	t.Cleanup(func() { // stop any stray fake git by its exact PID
		b, _ := os.ReadFile(pids)
		for _, s := range strings.Fields(string(b)) {
			if p, err := strconv.Atoi(s); err == nil {
				_ = syscall.Kill(p, syscall.SIGKILL)
			}
		}
	})
	var mu sync.Mutex
	spawned := 0
	restore := proc.SetSpawnHook(func(string, []string) { mu.Lock(); spawned++; mu.Unlock() })
	defer restore()
	proc.Group()
	defer proc.Release()

	c, err := NewReadClone(t.TempDir(), "mem-other", "")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); c.Head() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, _ := os.ReadFile(pids); len(strings.Fields(string(b))) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	b, _ := os.ReadFile(pids)
	if len(strings.Fields(string(b))) == 0 {
		t.Fatal("the fake git never ran")
	}
	mu.Lock()
	n := spawned
	mu.Unlock()
	if n != 1 {
		t.Errorf("proc's spawn seam saw %d git spawns for one Head, want 1", n)
	}
	proc.KillAll()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Head still waiting 3s after KillAll: its git child is not under proc")
	}
	time.Sleep(200 * time.Millisecond)
	for _, s := range strings.Fields(string(b)) {
		if p, err := strconv.Atoi(s); err == nil && syscall.Kill(p, 0) == nil {
			t.Errorf("git child %d still running after KillAll", p)
		}
	}
}

// A fetch that times out kills git's whole process group, so the ssh child a
// real fetch starts does not outlive it. Here git is a script that starts a
// sleeping child of its own.
func TestFetchTimeoutKillsGitsChildren(t *testing.T) {
	bin := t.TempDir()
	childPID := filepath.Join(bin, "child.pid")
	script := "#!/bin/sh\nsleep 60 &\necho $! > " + childPID + "\nwait\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { // stop a stray child by its exact PID
		b, _ := os.ReadFile(childPID)
		if p, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			_ = syscall.Kill(p, syscall.SIGKILL)
		}
	})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GIT_SSH_COMMAND", "ssh")
	c, err := NewReadClone(t.TempDir(), "mem-other", "git@github.com:me/mem-other.git")
	if err != nil {
		t.Fatal(err)
	}
	c.Timeout = 400 * time.Millisecond
	if err := c.Fetch(); err == nil {
		t.Fatal("Fetch succeeded with a git that hangs")
	}
	b, _ := os.ReadFile(childPID)
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("the fake git's child never started: %q", b)
	}
	for i := 0; i < 50 && syscall.Kill(pid, 0) == nil; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if syscall.Kill(pid, 0) == nil {
		t.Errorf("git's child %d still running after the fetch timed out", pid)
	}
}
