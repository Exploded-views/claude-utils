package main

import (
	"errors"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/lock"
	"github.com/MarimerLLC/claude-utils/internal/secretcmd"
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

// The single-repo daemon takes the sync dir's lock exactly once (startSingle):
// a second take of it in the same process would fail with ErrHeld and the
// daemon would never start. While it runs nobody else gets the lock, and Stop
// gives it back.
func TestProgramStart_SingleRepoTakesTheLockOnceAndStopReleasesIt(t *testing.T) {
	cfg := guardCfg(t, privateH(), false)
	cfg.PrivateOnly = false
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	p := &program{configPath: cfg.Path()}
	if err := p.Start(nil); err != nil {
		t.Fatalf("a single daemon must start (a double lock take would refuse): %v", err)
	}
	if _, err := lock.Acquire(cfg.SyncDir); !errors.Is(err, lock.ErrHeld) {
		t.Fatalf("lock while the daemon runs: %v, want ErrHeld", err)
	}
	if err := (&program{configPath: cfg.Path()}).Start(nil); !errors.Is(err, lock.ErrHeld) {
		t.Fatalf("second daemon: %v, want ErrHeld", err)
	}
	if err := p.Stop(nil); err != nil {
		t.Fatal(err)
	}
	l, err := lock.Acquire(cfg.SyncDir)
	if err != nil {
		t.Fatalf("after Stop the lock must be free: %v", err)
	}
	l.Release()
}

// The daemon asks nobody to approve a prompt, so its secret commands keep the
// short timeout; starting it must switch the process to that mode.
func TestProgramStart_MarksTheProcessAsTheDaemonForSecretCommands(t *testing.T) {
	cfg := guardCfg(t, privateH(), false)
	cfg.PrivateOnly = false
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv(secretcmd.TimeoutEnv, "")
	t.Cleanup(func() { secretcmd.SetDaemon(false) })
	p := &program{configPath: cfg.Path()}
	if err := p.Start(nil); err != nil {
		t.Fatal(err)
	}
	defer p.Stop(nil)
	if got := secretcmd.KeyCmdTimeout(); got != 10*time.Second {
		t.Fatalf("daemon key command timeout = %v, want 10s", got)
	}
}
