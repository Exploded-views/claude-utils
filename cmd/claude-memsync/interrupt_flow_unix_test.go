//go:build !windows

package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/proc"
	"github.com/MarimerLLC/claude-utils/internal/related"
)

// flowHarness wires the real guard and the real sync flow to two signal
// channels (the OS delivers a signal to every registered channel), a fake
// service and a fake git that sleeps.
type flowHarness struct {
	t      *testing.T
	guard  chan os.Signal // what guardChildren listens on
	flow   chan os.Signal // what withSyncLocksStopping listens on
	exited chan int
	pids   string
	dir    string
	f      *fakeSvc
}

func newFlowHarness(t *testing.T) *flowHarness {
	t.Helper()
	f := &fakeSvc{status: "running"}
	installFakeSvc(t, f)
	h := &flowHarness{t: t, guard: make(chan os.Signal, 4), flow: make(chan os.Signal, 4), exited: make(chan int, 4), f: f}
	h.dir = heldLock(t, f)
	bin := t.TempDir()
	h.pids = filepath.Join(bin, "pids")
	// Only the related-repo git (it carries core.hooksPath) sleeps; the flow's
	// own git calls (rekey guard, markers) reach the real git.
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ncase \"$*\" in *core.hooksPath*) echo $$ >> " + h.pids + "; exec sleep 60;; esac\nexec " + realGit + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GIT_SSH_COMMAND", "")
	t.Cleanup(func() { // stop any stray fake git by its exact PID
		b, _ := os.ReadFile(h.pids)
		for _, s := range strings.Fields(string(b)) {
			if p, err := strconv.Atoi(s); err == nil {
				_ = syscall.Kill(p, syscall.SIGKILL)
			}
		}
	})
	t.Cleanup(proc.Release)
	var mu sync.Mutex
	calls := 0
	notifySignals = func() (<-chan os.Signal, func()) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return h.guard, func() {}
		}
		return h.flow, func() {}
	}
	exitFn = func(c int) { h.exited <- c }
	return h
}

// startGit runs a related-repo git (through proc) and returns its fake's PID.
func (h *flowHarness) startGit() int {
	h.t.Helper()
	before := len(strings.Fields(h.readPids()))
	c, err := related.NewReadClone(h.t.TempDir(), "mem-other", "")
	if err != nil {
		h.t.Fatal(err)
	}
	go c.Head()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if f := strings.Fields(h.readPids()); len(f) > before {
			pid, _ := strconv.Atoi(f[before])
			return pid
		}
	}
	h.t.Fatal("the fake git never ran")
	return 0
}

func (h *flowHarness) readPids() string { b, _ := os.ReadFile(h.pids); return string(b) }

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func (h *flowHarness) goneWithin(pid int, d time.Duration) bool {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if !alive(pid) {
			return true
		}
	}
	return !alive(pid)
}

// H3: while the sync flow owns the signals, the first one leaves a running git
// alone and the change completes; only the second kills it and exits 130.
func TestInterruptDuringSyncFlow_FirstSignalLeavesGitAlone(t *testing.T) {
	h := newFlowHarness(t)
	stop := guardChildren()
	defer stop()
	var pid int
	completed := false
	err := withSyncLocksStopping(&bytes.Buffer{}, []string{h.dir}, func() error {
		pid = h.startGit()
		h.guard <- syscall.SIGINT
		h.flow <- syscall.SIGINT
		time.Sleep(300 * time.Millisecond)
		if !alive(pid) {
			t.Error("the first signal killed the change's git")
		}
		select {
		case c := <-h.exited:
			t.Errorf("the first signal exited (%d)", c)
		default:
		}
		completed = true
		return nil
	})
	// The change ran to its end; an interrupted flow still reports errInterrupted.
	if (err != nil && !errors.Is(err, errInterrupted)) || !completed {
		t.Fatalf("err=%v completed=%v", err, completed)
	}
}

func TestInterruptDuringSyncFlow_SecondSignalKillsGitAndExits130(t *testing.T) {
	h := newFlowHarness(t)
	stop := guardChildren()
	defer stop()
	var pid int
	_ = withSyncLocksStopping(&bytes.Buffer{}, []string{h.dir}, func() error {
		pid = h.startGit()
		for i := 0; i < 2; i++ {
			h.guard <- syscall.SIGINT
			h.flow <- syscall.SIGINT
		}
		select {
		case c := <-h.exited:
			if c != 130 {
				t.Errorf("exit code %d, want 130", c)
			}
		case <-time.After(3 * time.Second):
			t.Error("the second signal did not exit")
		}
		return nil
	})
	if !h.goneWithin(pid, 2*time.Second) {
		t.Errorf("git %d still running after the second signal", pid)
	}
}

// M6: a signal the guard ignores during the flow must not end the guard: one
// after the flow kills the child and exits.
func TestInterruptGuardOutlivesTheSyncFlow(t *testing.T) {
	h := newFlowHarness(t)
	stop := guardChildren()
	defer stop()
	err := withSyncLocksStopping(&bytes.Buffer{}, []string{h.dir}, func() error {
		h.guard <- syscall.SIGINT // ignored by the guard: the flow owns it
		for deadline := time.Now().Add(2 * time.Second); len(h.guard) > 0 && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		}
		time.Sleep(100 * time.Millisecond)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-h.exited:
		t.Fatalf("exited (%d) on a signal during the flow", c)
	default:
	}
	pid := h.startGit()
	h.guard <- syscall.SIGINT
	select {
	case c := <-h.exited:
		if c != 130 {
			t.Errorf("exit code %d, want 130", c)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a signal after the flow did not exit: the guard stopped listening")
	}
	if !h.goneWithin(pid, 2*time.Second) {
		t.Errorf("git %d still running after the signal", pid)
	}
}
