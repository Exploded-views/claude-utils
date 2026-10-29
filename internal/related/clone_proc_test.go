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
