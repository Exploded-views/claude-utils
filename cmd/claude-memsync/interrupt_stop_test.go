package main

import (
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/proc"
)

// stopGuardFixture swaps the guard's seams for one signal channel and an exit
// that records its code; it restores them on cleanup.
func stopGuardFixture(t *testing.T) (sig chan os.Signal, exited chan int) {
	t.Helper()
	oN, oE := notifySignals, exitFn
	t.Cleanup(func() { notifySignals, exitFn = oN, oE })
	t.Cleanup(proc.Release) // KillAll makes Run refuse children until released
	sig = make(chan os.Signal, 2)
	exited = make(chan int, 2)
	notifySignals = func() (<-chan os.Signal, func()) { return sig, func() {} }
	exitFn = func(c int) { exited <- c }
	return sig, exited
}

// returns reports whether f returns; the bound only turns a deadlock into a
// failure, a passing run never waits for it.
func returns(f func()) bool {
	ok := make(chan struct{})
	go func() { f(); close(ok) }()
	select {
	case <-ok:
		return true
	case <-time.After(10 * time.Second):
		return false
	}
}

// stop() must join the guard goroutine: once it returns, nothing of the guard
// reads the package seams, so a test (or caller) may reassign them. A signal
// already queued when stop() runs is the case that exposed it: the goroutine
// could still be about to read exitFn.
func TestGuardStopJoinsTheGoroutine(t *testing.T) {
	sig, exited := stopGuardFixture(t)
	for i := 0; i < 200; i++ {
		exitFn = func(c int) { exited <- c }
		stop := guardChildren()
		sig <- syscall.SIGINT
		stop()
		exitFn = func(int) {} // the write the late read raced with
		for j := 0; j < 20; j++ {
			runtime.Gosched()
		}
		for len(exited) > 0 {
			<-exited
		}
		proc.Release()
	}
}

// stop() must return even when the guard goroutine is inside exitFn, which in
// production never returns and in a test may block.
func TestGuardStopReturnsWhileExitIsBlocked(t *testing.T) {
	sig, _ := stopGuardFixture(t)
	inExit, release := make(chan struct{}), make(chan struct{})
	exitFn = func(int) { close(inExit); <-release }
	t.Cleanup(func() { close(release) })
	stop := guardChildren()
	sig <- syscall.SIGTERM
	<-inExit
	if !returns(stop) {
		t.Fatal("stop() blocked while the guard was inside exitFn")
	}
}

// While the sync flow owns the signals the guard swallows them; stop() must
// still return, with a signal just delivered, and again when called twice.
func TestGuardStopReturnsWhenTheSyncFlowOwnsSignals(t *testing.T) {
	sig, _ := stopGuardFixture(t)
	syncOwnsSignals.Store(true)
	t.Cleanup(func() { syncOwnsSignals.Store(false) })
	stop := guardChildren()
	sig <- syscall.SIGINT
	sig <- syscall.SIGTERM
	if !returns(stop) {
		t.Fatal("stop() blocked while the sync flow owned the signals")
	}
	if !returns(stop) {
		t.Fatal("a second stop() blocked")
	}
}

// stop() is idempotent: the notifier is stopped once, and repeats are no-ops.
func TestGuardStopIsIdempotent(t *testing.T) {
	stopSigs := 0
	_, _ = stopGuardFixture(t)
	notifySignals = func() (<-chan os.Signal, func()) { return make(chan os.Signal), func() { stopSigs++ } }
	stop := guardChildren()
	for i := 0; i < 3; i++ {
		if !returns(stop) {
			t.Fatalf("stop() call %d blocked", i+1)
		}
	}
	if stopSigs != 1 {
		t.Fatalf("signal notifier stopped %d times, want 1", stopSigs)
	}
}
