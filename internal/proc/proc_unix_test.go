//go:build !windows

package proc

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// KillAll kills a running child and the processes it started, and Run then
// refuses new ones. Liveness is checked with signal 0.
func TestKillAllKillsTheChildAndItsGroup(t *testing.T) {
	Group()
	defer Release()
	pids := filepath.Join(t.TempDir(), "pids")
	// The shell starts a background sleeper, records both PIDs, then waits.
	script := "sleep 60 & echo $$ $! > " + pids + "; wait"
	cmd := Command("sh", "-c", script)
	errc := make(chan error, 1)
	go func() { errc <- Run(cmd) }()
	var shell, sleeper int
	for i := 0; i < 100 && sleeper == 0; i++ {
		if b, _ := os.ReadFile(pids); strings.Count(string(b), "\n") > 0 {
			f := strings.Fields(string(b))
			shell, _ = strconv.Atoi(f[0])
			sleeper, _ = strconv.Atoi(f[1])
		} else {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if sleeper == 0 {
		t.Fatal("child did not start")
	}
	KillAll()
	select {
	case <-errc:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after KillAll")
	}
	for _, p := range []int{shell, sleeper} {
		for i := 0; i < 50 && syscall.Kill(p, 0) == nil; i++ {
			time.Sleep(20 * time.Millisecond)
		}
		if syscall.Kill(p, 0) == nil {
			_ = syscall.Kill(p, syscall.SIGKILL)
			t.Errorf("pid %d still running after KillAll", p)
		}
	}
	if err := Run(exec.Command("true")); err != ErrStopped {
		t.Errorf("Run after KillAll = %v, want ErrStopped", err)
	}
}
