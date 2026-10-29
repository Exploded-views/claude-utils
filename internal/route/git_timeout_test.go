//go:build !windows

package route

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// One hung git (a stalled mount, an fsmonitor hook) must not hold up Warm, and
// so Start, for longer than the per-call timeout: the folder is unresolved for
// this generation, not "no repo", and resolves again after a Refresh.
func TestHungGitDoesNotBlockWarm(t *testing.T) {
	gitEnv(t)
	root := t.TempDir()
	var folders []string
	for _, n := range []string{"a", "b", "c", "d", "e", "f"} {
		d := filepath.Join(root, n)
		initRepo(t, d, "git@github.com:me/"+n+".git")
		folders = append(folders, escape(d))
	}
	realPath := os.Getenv("PATH")
	bin := t.TempDir()
	pids := filepath.Join(bin, "pids")
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\necho $$ >> "+pids+"\nexec sleep 60\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { // stop any stray fake git by its exact PID
		b, _ := os.ReadFile(pids)
		for _, s := range strings.Fields(string(b)) {
			if p, err := strconv.Atoi(s); err == nil {
				_ = syscall.Kill(p, syscall.SIGKILL)
			}
		}
	})
	t.Cleanup(SetGitTimeoutForTest(300 * time.Millisecond))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+realPath)

	r := New(fileOf(Rule{Remote: "github.com/me/*", Repo: "projects"}), func(string) bool { return true })
	r.Logf = func(string, ...any) {}
	done := make(chan struct{})
	go func() { defer close(done); r.Warm(folders) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("Warm still blocked 20s after the git calls hung")
	}
	for _, f := range folders {
		if d := r.Decide(f); d.Repo != "" || d.Reason != ReasonUnresolved {
			t.Errorf("hung git: %+v, want unresolved", d)
		}
	}
	b, _ := os.ReadFile(pids)
	started := strings.Fields(string(b))
	if len(started) == 0 {
		t.Fatal("the fake git never ran")
	}
	for _, s := range started {
		if p, err := strconv.Atoi(s); err == nil && syscall.Kill(p, 0) == nil {
			time.Sleep(300 * time.Millisecond)
			if syscall.Kill(p, 0) == nil {
				t.Errorf("hung git %d still running after its timeout", p)
			}
		}
	}

	t.Setenv("PATH", realPath)
	r.Refresh()
	for _, f := range folders {
		if d := r.Decide(f); d.Repo != "projects" {
			t.Errorf("after Refresh with a working git: %+v, want projects", d)
		}
	}
}

// Warm held to a deadline: more hung gits than semaphore slots, and
// WarmWithin still returns at the deadline, saying it did not finish. The
// folders keep resolving in the background and are decided when git answers.
func TestWarmWithinReturnsAtTheDeadline(t *testing.T) {
	gitEnv(t)
	root := t.TempDir()
	var folders []string
	for i := 0; i < 9; i++ { // more than the 4 git slots
		d := filepath.Join(root, "r"+strconv.Itoa(i))
		initRepo(t, d, "git@github.com:me/r"+strconv.Itoa(i)+".git")
		folders = append(folders, escape(d))
	}
	realPath := os.Getenv("PATH")
	bin := t.TempDir()
	pids := filepath.Join(bin, "pids")
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\necho $$ >> "+pids+"\nexec sleep 60\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { // stop any stray fake git by its exact PID
		b, _ := os.ReadFile(pids)
		for _, s := range strings.Fields(string(b)) {
			if p, err := strconv.Atoi(s); err == nil {
				_ = syscall.Kill(p, syscall.SIGKILL)
			}
		}
	})
	t.Cleanup(SetGitTimeoutForTest(2 * time.Second))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+realPath)
	r := New(fileOf(Rule{Remote: "github.com/me/*", Repo: "projects"}), func(string) bool { return true })
	r.Logf = func(string, ...any) {}
	start := time.Now()
	if r.WarmWithin(folders, 300*time.Millisecond) {
		t.Error("WarmWithin said it finished while every git hung")
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("WarmWithin took %v, want about its 300ms deadline", took)
	}
	// The workers go on: with a working git the rest of the folders get decided.
	t.Setenv("PATH", realPath)
	r.Refresh()
	if !r.WarmWithin(folders, 30*time.Second) {
		t.Fatal("a working git did not finish within 30s")
	}
	for _, f := range folders {
		if d := r.Decide(f); d.Repo != "projects" {
			t.Errorf("%+v, want projects", d)
		}
	}
}
