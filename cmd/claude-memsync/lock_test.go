package main

import (
	"errors"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/lock"
)

// The recipients CLI must not run while the daemon holds the sync dir.
func TestWithSyncLock_RefusedWhileDaemonRuns(t *testing.T) {
	dir := t.TempDir()
	daemon, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	ran := false
	err = withSyncLock(dir, func() error { ran = true; return nil })
	if !errors.Is(err, lock.ErrHeld) || ran {
		t.Fatalf("CLI ran under a held lock: ran=%v err=%v", ran, err)
	}
	daemon.Release()
	if err := withSyncLock(dir, func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("after release: %v ran=%v", err, ran)
	}
	// And the CLI's own lock keeps a daemon out while it works.
	err = withSyncLock(dir, func() error {
		if _, e := lock.Acquire(dir); !errors.Is(e, lock.ErrHeld) {
			t.Errorf("daemon got the lock during the CLI action: %v", e)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
