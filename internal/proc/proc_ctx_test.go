//go:build !windows

package proc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestSpawnHooksSeeEveryRunAndAreRemovable(t *testing.T) {
	var starts, ends atomic.Int64
	var gotArgs []string
	r1 := SetSpawnHook(func(name string, args []string) { starts.Add(1); gotArgs = args })
	r2 := SetSpawnEndHook(func(string, []string) { ends.Add(1) })
	if err := Run(Command("sh", "-c", "exit 0")); err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 1 || ends.Load() != 1 || len(gotArgs) != 2 || gotArgs[1] != "exit 0" {
		t.Errorf("starts=%d ends=%d args=%v", starts.Load(), ends.Load(), gotArgs)
	}
	// A child that fails to start is still counted as started and ended.
	if err := Run(exec.Command("/nonexistent/binary-xyz")); err == nil {
		t.Error("starting a missing binary succeeded")
	}
	if starts.Load() != 2 || ends.Load() != 2 {
		t.Errorf("after a failed start: starts=%d ends=%d", starts.Load(), ends.Load())
	}
	r1()
	r2()
	if err := Run(Command("true")); err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 2 || ends.Load() != 2 {
		t.Errorf("hooks fired after removal: starts=%d ends=%d", starts.Load(), ends.Load())
	}
}

func TestRunReturnsTheChildsExitError(t *testing.T) {
	err := Run(Command("sh", "-c", "exit 3"))
	var ee *exec.ExitError
	if !asExit(err, &ee) || ee.ExitCode() != 3 {
		t.Errorf("Run = %v, want exit status 3", err)
	}
}

func asExit(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

func TestCommandContextUngroupedIsPlainAndGroupedLeadsItsOwnGroup(t *testing.T) {
	c := CommandContext(context.Background(), "true")
	if c.SysProcAttr != nil && c.SysProcAttr.Setpgid {
		t.Error("an ungrouped CommandContext set a process group")
	}
	Group()
	defer Release()
	g := CommandContext(context.Background(), "true")
	if g.SysProcAttr == nil || !g.SysProcAttr.Setpgid {
		t.Error("after Group, CommandContext did not set a process group")
	}
	if g.Cancel == nil {
		t.Error("after Group, CommandContext has no group-killing Cancel")
	}
}

// Cancelling a grouped command kills the child's own children too.
func TestGroupedCommandContextCancelKillsTheWholeGroup(t *testing.T) {
	pids := filepath.Join(t.TempDir(), "pids")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := GroupedCommandContext(ctx, "sh", "-c", "sleep 60 & echo $$ $! > "+pids+"; wait")
	errc := make(chan error, 1)
	go func() { errc <- Run(cmd) }()
	var pp []int
	for i := 0; i < 150 && len(pp) < 2; i++ {
		if b, _ := os.ReadFile(pids); strings.Count(string(b), "\n") > 0 {
			for _, f := range strings.Fields(string(b)) {
				n, _ := strconv.Atoi(f)
				pp = append(pp, n)
			}
		} else {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if len(pp) < 2 {
		t.Fatal("child did not start")
	}
	cancel()
	select {
	case err := <-errc:
		if err == nil {
			t.Error("a cancelled command reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	for _, p := range pp {
		for i := 0; i < 50 && syscall.Kill(p, 0) == nil; i++ {
			time.Sleep(20 * time.Millisecond)
		}
		if syscall.Kill(p, 0) == nil {
			_ = syscall.Kill(p, syscall.SIGKILL)
			t.Errorf("pid %d survived the cancel", p)
		}
	}
}

// A child that is not a group leader is killed alone.
func TestKillGroupFallsBackToTheSingleProcess(t *testing.T) {
	cmd := exec.Command("sleep", "60") // no Setpgid: not a leader, -pid has no group
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	killGroup(cmd.Process.Pid)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("killed process exited cleanly")
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("process survived killGroup")
	}
}

func TestGroupReenablesRunAfterKillAll(t *testing.T) {
	KillAll()
	if err := Run(Command("true")); err != ErrStopped {
		t.Fatalf("Run after KillAll = %v, want ErrStopped", err)
	}
	Group()
	defer Release()
	if err := Run(Command("true")); err != nil {
		t.Errorf("Run after Group = %v", err)
	}
	KillAll()
	Release()
	if err := Run(Command("true")); err != nil {
		t.Errorf("Run after Release = %v", err)
	}
}
