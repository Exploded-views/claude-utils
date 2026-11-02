package lock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSecondDaemonOnSameSyncDirRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sync")
	l1, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(dir); !errors.Is(err, ErrHeld) {
		t.Fatalf("second Acquire = %v, want ErrHeld", err)
	}
	// A different sync dir is independent.
	l2, err := Acquire(filepath.Join(t.TempDir(), "other"))
	if err != nil {
		t.Fatal(err)
	}
	l2.Release()

	l1.Release()
	l3, err := Acquire(dir)
	if err != nil {
		t.Fatalf("after Release: %v", err)
	}
	l3.Release()
}

func TestHookLockIsNonBlockingAndReleases(t *testing.T) {
	dir := t.TempDir()
	l, held, err := TryHook(dir)
	if err != nil || held || l == nil {
		t.Fatalf("first: %v %v %v", l, held, err)
	}
	if l2, held, err := TryHook(dir); err != nil || !held || l2 != nil {
		t.Fatalf("second while held: %v %v %v", l2, held, err)
	}
	l.Release()
	l3, held, err := TryHook(dir)
	if err != nil || held {
		t.Fatalf("after release: held=%v err=%v", held, err)
	}
	l3.Release()
}

func TestWaitFile_TimesOutWhileHeldThenSucceedsAfterRelease(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	open := func() *os.File {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		return f
	}
	rel1, err := WaitFile(open(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := WaitFile(open(), 100*time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Fatalf("held: err = %v, want ErrTimeout", err)
	}
	if d := time.Since(start); d < 90*time.Millisecond || d > 3*time.Second {
		t.Fatalf("timeout took %v", d)
	}
	rel1()
	rel2, err := WaitFile(open(), time.Second)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	rel2()
}

func TestWaitFile_WaitsForReleaseWithinDeadline(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	f1, _ := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0600)
	f2, _ := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0600)
	defer f1.Close()
	defer f2.Close()
	rel1, err := WaitFile(f1, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(100 * time.Millisecond); rel1() }()
	rel2, err := WaitFile(f2, 5*time.Second)
	if err != nil {
		t.Fatalf("second WaitFile = %v, want success once released", err)
	}
	rel2()
}

// A hard error from the OS lock call is returned at once, not retried until
// the deadline and reported as a timeout.
func TestWaitFile_HardErrorIsNotATimeout(t *testing.T) {
	old := tryLockFn
	defer func() { tryLockFn = old }()
	hard := errors.New("operation not supported")
	tryLockFn = func(*os.File) error { return hard }
	f, _ := os.OpenFile(filepath.Join(t.TempDir(), "f"), os.O_CREATE|os.O_RDWR, 0600)
	defer f.Close()
	start := time.Now()
	_, err := WaitFile(f, 5*time.Second)
	if !errors.Is(err, hard) || errors.Is(err, ErrTimeout) || time.Since(start) > time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
}
