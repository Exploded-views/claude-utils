package proc

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"sync/atomic"
)

// A process that must not outlive a deadline (the changes hook) starts its
// children through Command and Run, after Group: each child then leads its own
// process group, and KillAll kills the groups still running and refuses any
// new child. Other callers get a plain exec.Cmd and are not affected.

var (
	mu      sync.Mutex
	running = map[int]struct{}{}
	grouped atomic.Bool
	stopped atomic.Bool
)

// ErrStopped is what Run returns once KillAll was called.
var ErrStopped = errors.New("stopped: the deadline passed")

// Group makes later children lead their own process group, and lets Run start
// children again after an earlier KillAll (one process runs one hook; tests
// run several).
func Group() {
	grouped.Store(true)
	stopped.Store(false)
}

// Release undoes Group and KillAll: a process that ran one hook exits, but a
// test goes on to run git.
func Release() {
	grouped.Store(false)
	stopped.Store(false)
}

// Command is exec.Command with HideWindow, and a process group of its own
// after Group.
func Command(name string, args ...string) *exec.Cmd {
	c := exec.Command(name, args...)
	HideWindow(c)
	if grouped.Load() {
		setGroup(c)
	}
	return c
}

// CommandContext is Command with exec.CommandContext's cancellation; after
// Group, cancelling kills the child's whole process group.
func CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, name, args...)
	HideWindow(c)
	if grouped.Load() {
		setGroup(c)
		c.Cancel = func() error {
			if c.Process != nil {
				killGroup(c.Process.Pid)
			}
			return nil
		}
	}
	return c
}

var spawnHook atomic.Pointer[func(name string, args []string)]

// SetSpawnHook calls f for every child Run starts (tests count spawns with it)
// and returns the function that removes it.
func SetSpawnHook(f func(name string, args []string)) (restore func()) {
	spawnHook.Store(&f)
	return func() { spawnHook.Store(nil) }
}

// Run is cmd.Run that KillAll can find.
func Run(cmd *exec.Cmd) error {
	if stopped.Load() {
		return ErrStopped
	}
	if h := spawnHook.Load(); h != nil {
		(*h)(cmd.Path, cmd.Args[1:])
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	mu.Lock()
	running[pid] = struct{}{}
	mu.Unlock()
	if stopped.Load() { // KillAll ran between the check and the registration
		killGroup(pid)
	}
	err := cmd.Wait()
	mu.Lock()
	delete(running, pid)
	mu.Unlock()
	return err
}

// KillAll kills the process group of every child still running and makes Run
// refuse new ones.
func KillAll() {
	stopped.Store(true)
	mu.Lock()
	pids := make([]int, 0, len(running))
	for p := range running {
		pids = append(pids, p)
	}
	mu.Unlock()
	for _, p := range pids {
		killGroup(p)
	}
}
