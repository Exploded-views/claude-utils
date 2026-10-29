package lock

import (
	"errors"
	"path/filepath"
	"testing"
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
