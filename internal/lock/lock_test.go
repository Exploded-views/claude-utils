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
