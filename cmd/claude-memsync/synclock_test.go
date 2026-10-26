package main

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/lock"
	"github.com/kardianos/service"
)

// fakeSvc swaps the service functions for recorders, so no test ever touches
// a real service. status is what `status` reports; stopping calls onStop.
type fakeSvc struct {
	calls  []string
	status string
	err    error
	onStop func()
	stopEr error
}

func installFakeSvc(t *testing.T, f *fakeSvc) {
	t.Helper()
	oS, oP, oT := startFn, stopFn, statusFn
	oW, oL, oRS := lockWait, lockPoll, restartSettle
	oN, oE := notifySignals, exitFn
	t.Cleanup(func() {
		startFn, stopFn, statusFn = oS, oP, oT
		lockWait, lockPoll, restartSettle = oW, oL, oRS
		notifySignals, exitFn = oN, oE
	})
	statusFn = func() (string, error) { f.calls = append(f.calls, "status"); return f.status, f.err }
	stopFn = func() error {
		f.calls = append(f.calls, "stop")
		if f.onStop != nil {
			f.onStop()
		}
		return f.stopEr
	}
	startFn = func() error { f.calls = append(f.calls, "start"); return nil }
	lockWait, lockPoll, restartSettle = 2*time.Second, 5*time.Millisecond, 0
	notifySignals = func() (<-chan os.Signal, func()) { return make(chan os.Signal), func() {} }
	exitFn = func(int) { t.Error("exitFn called unexpectedly") }
}

func (f *fakeSvc) did() string {
	var out []string
	for _, c := range f.calls {
		if c != "status" {
			out = append(out, c)
		}
	}
	return strings.Join(out, ",")
}

// heldLock models the daemon: it holds the sync dir's lock until it is stopped.
func heldLock(t *testing.T, f *fakeSvc) string {
	t.Helper()
	dir := t.TempDir()
	l, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	f.onStop = l.Release
	t.Cleanup(l.Release)
	return dir
}

func TestSyncLockStopping_RunningServiceIsStoppedThenRestarted(t *testing.T) {
	f := &fakeSvc{status: "running"}
	installFakeSvc(t, f)
	dir := heldLock(t, f)
	var errb bytes.Buffer
	ran := ""
	err := withSyncLockStopping(&errb, dir, func() error { ran = f.did(); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if ran != "stop" || f.did() != "stop,start" {
		t.Fatalf("order: during fn %q, overall %q; want stop, run, start", ran, f.did())
	}
	for _, w := range []string{"stopping the claude-memsync service for this change…", "restarted"} {
		if !strings.Contains(errb.String(), w) {
			t.Errorf("missing %q in %q", w, errb.String())
		}
	}
	if l, err := lock.Acquire(dir); err != nil {
		t.Errorf("the lock must be free afterwards: %v", err)
	} else {
		l.Release()
	}
}

func TestSyncLockStopping_ErrorInsideStillRestarts(t *testing.T) {
	f := &fakeSvc{status: "running"}
	installFakeSvc(t, f)
	dir := heldLock(t, f)
	boom := errors.New("boom")
	err := withSyncLockStopping(&bytes.Buffer{}, dir, func() error { return boom })
	if !errors.Is(err, boom) || f.did() != "stop,start" {
		t.Fatalf("err=%v calls=%q", err, f.did())
	}
}

func TestSyncLockStopping_PanicInsideStillRestarts(t *testing.T) {
	f := &fakeSvc{status: "running"}
	installFakeSvc(t, f)
	dir := heldLock(t, f)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic must propagate")
			}
		}()
		_ = withSyncLockStopping(&bytes.Buffer{}, dir, func() error { panic("kaboom") })
	}()
	if f.did() != "stop,start" {
		t.Fatalf("calls=%q", f.did())
	}
}

func TestSyncLockStopping_SignalWhileStoppedStillRestarts(t *testing.T) {
	f := &fakeSvc{status: "running"}
	installFakeSvc(t, f)
	dir := heldLock(t, f)
	sig := make(chan os.Signal, 2)
	notifySignals = func() (<-chan os.Signal, func()) { return sig, func() {} }
	finished := false
	err := withSyncLockStopping(&bytes.Buffer{}, dir, func() error {
		sig <- syscall.SIGTERM
		time.Sleep(50 * time.Millisecond) // the handler sees it while fn is running
		finished = true
		return nil
	})
	if !finished {
		t.Error("the first signal must let the change finish, not cut it in half")
	}
	if !errors.Is(err, errInterrupted) || f.did() != "stop,start" {
		t.Fatalf("err=%v calls=%q", err, f.did())
	}
}

func TestSyncLockStopping_SecondSignalRestartsNowAndExits(t *testing.T) {
	f := &fakeSvc{status: "running"}
	installFakeSvc(t, f)
	dir := heldLock(t, f)
	sig := make(chan os.Signal, 2)
	notifySignals = func() (<-chan os.Signal, func()) { return sig, func() {} }
	exited := make(chan int, 1)
	exitFn = func(c int) { exited <- c }
	_ = withSyncLockStopping(&bytes.Buffer{}, dir, func() error {
		sig <- syscall.SIGINT
		sig <- syscall.SIGINT
		select {
		case <-exited:
			exited <- 130
		case <-time.After(2 * time.Second):
			t.Error("a second signal did not exit")
		}
		return nil
	})
	if c := <-exited; c != 130 {
		t.Errorf("exit code %d, want 130", c)
	}
	if f.did() != "stop,start" {
		t.Fatalf("the service must be started exactly once: %q", f.did())
	}
}

func TestSyncLockStopping_SignalWhileWaitingForTheLockAbortsAndRestarts(t *testing.T) {
	f := &fakeSvc{status: "running"}
	installFakeSvc(t, f)
	dir := t.TempDir()
	l, err := lock.Acquire(dir) // never released: the wait cannot succeed
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	sig := make(chan os.Signal, 1)
	notifySignals = func() (<-chan os.Signal, func()) { return sig, func() {} }
	f.onStop = func() { sig <- syscall.SIGINT }
	ran := false
	err = withSyncLockStopping(&bytes.Buffer{}, dir, func() error { ran = true; return nil })
	if ran || !errors.Is(err, errInterrupted) || f.did() != "stop,start" {
		t.Fatalf("ran=%v err=%v calls=%q", ran, err, f.did())
	}
}

func TestSyncLockStopping_WaitIsBoundedAndRestarts(t *testing.T) {
	f := &fakeSvc{status: "running"}
	installFakeSvc(t, f)
	dir := t.TempDir()
	l, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	lockWait = 100 * time.Millisecond
	ran := false
	err = withSyncLockStopping(&bytes.Buffer{}, dir, func() error { ran = true; return nil })
	if ran || !errors.Is(err, lock.ErrHeld) || f.did() != "stop,start" {
		t.Fatalf("ran=%v err=%v calls=%q", ran, err, f.did())
	}
}

// A lock held by something other than a running service is refused as before,
// and nothing is started or stopped.
func TestSyncLockStopping_ServiceNotRunningRefusesAndTouchesNothing(t *testing.T) {
	for name, f := range map[string]*fakeSvc{
		"stopped":       {status: "stopped"},
		"unknown":       {status: "unknown"},
		"not installed": {status: "unknown", err: service.ErrNotInstalled},
		"status fails":  {status: "unknown", err: errors.New("no systemd")},
	} {
		installFakeSvc(t, f)
		dir := t.TempDir()
		l, err := lock.Acquire(dir)
		if err != nil {
			t.Fatal(err)
		}
		ran := false
		err = withSyncLockStopping(&bytes.Buffer{}, dir, func() error { ran = true; return nil })
		l.Release()
		if ran || !errors.Is(err, lock.ErrHeld) || f.did() != "" {
			t.Errorf("%s: ran=%v err=%v calls=%q", name, ran, err, f.did())
		}
	}
}

func TestSyncLockStopping_FreeLockRunsWithoutTouchingTheService(t *testing.T) {
	f := &fakeSvc{status: "running"}
	installFakeSvc(t, f)
	ran := false
	if err := withSyncLockStopping(&bytes.Buffer{}, t.TempDir(), func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("ran=%v err=%v", ran, err)
	}
	if len(f.calls) != 0 {
		t.Errorf("service touched: %v", f.calls)
	}
}

// The commands really use it: recipients add against a daemon-held lock stops
// the (fake) service, adds the key and starts it again.
func TestRecipientsAdd_StopsAndRestartsTheRunningService(t *testing.T) {
	cfgs := routedHost(t, []string{"personal", "projects"}, "personal", nil)
	proj := encryptRouted(t, cfgs["projects"])
	f := &fakeSvc{status: "running"}
	installFakeSvc(t, f)
	held, err := lock.Acquire(proj.SyncDir)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	f.onStop = held.Release
	key := newAgeKey(t)
	code, _, errs := runRcpt("--repo", "projects", "add", key)
	if code != 0 || f.did() != "stop,start" {
		t.Fatalf("exit %d calls %q: %s", code, f.did(), errs)
	}
	if _, out, _ := runRcpt("--repo", "projects", "list"); !strings.Contains(out, key) {
		t.Errorf("key not added:\n%s", out)
	}
}
