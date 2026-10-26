package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/lock"
	syncpkg "github.com/MarimerLLC/claude-utils/internal/sync"
)

// How long a command waits for a stopped service to give the lock back, and how
// often it looks. Variables so tests stay fast.
var (
	lockWait = 30 * time.Second
	lockPoll = 200 * time.Millisecond
	// How long after the automatic restart before the service is asked
	// whether it is still running.
	restartSettle = time.Second
)

// notifySignals reports SIGINT and SIGTERM until the returned func is called;
// exitFn ends the process. Both are swapped by tests.
var (
	notifySignals = func() (<-chan os.Signal, func()) {
		ch := make(chan os.Signal, 2)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
		return ch, func() { signal.Stop(ch) }
	}
	exitFn = os.Exit
)

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// rekeyBlockedError refuses an interactive change to a half-migrated repo.
func rekeyBlockedError(syncDir, why string) error {
	name := filepath.Base(syncDir)
	repoFlag := " --repo " + name
	if name == ".claudesync" || name == "." || name == string(filepath.Separator) {
		repoFlag = "" // the single default repo needs no --repo
	}
	return fmt.Errorf("repo %s has an unfinished post-quantum rekey (%s): finish or roll back the rekey first (`claude-memsync recipients rekey-pq%s --apply` or `claude-memsync recipients rekey-pq%s --rollback`)", name, why, repoFlag, repoFlag)
}

// errInterrupted means a SIGINT or SIGTERM arrived while the service was stopped.
var errInterrupted = errors.New("interrupted")

// withSyncLockStopping is withSyncLock for interactive commands. When the lock
// is held and the claude-memsync service is installed and running, it stops the
// service, waits (bounded) for the lock, runs fn, and starts the service again
// on every way out: success, error, panic or a signal. When the lock is held
// and the service is not running, nothing is started or stopped and the
// ordinary refusal (lock.ErrHeld) is returned. A service that was not running
// is never started.
func withSyncLockStopping(stderr io.Writer, syncDir string, fn func() error) error {
	// Before the service is stopped or the lock taken: a repo in the middle of
	// a post-quantum migration must not be changed or pushed by anything but
	// the rekey itself (the daemon is blocked on it for the same reason).
	if why, blocked := agecrypt.RekeyBlocksSync(gitwt.New(syncDir)); blocked {
		return rekeyBlockedError(syncDir, why)
	}
	stderr = &lockedWriter{w: stderr} // the signal goroutine and the caller both print
	l, err := lock.Acquire(syncDir)
	if err == nil {
		defer l.Release()
		return fn()
	}
	if !errors.Is(err, lock.ErrHeld) {
		return err
	}
	if st, serr := statusFn(); serr != nil || st != "running" {
		return err // held by something else: refuse, touch nothing
	}

	sigs, stopSigs := notifySignals()
	defer stopSigs()

	fmt.Fprintln(stderr, "stopping the claude-memsync service for this change…")
	var once sync.Once
	restart := func() {
		once.Do(func() {
			if err := startFn(); err != nil {
				fmt.Fprintln(stderr, "could not restart the claude-memsync service:", err, "- run `claude-memsync start`")
				return
			}
			fmt.Fprintln(stderr, "claude-memsync service restarted")
		})
	}
	// Whatever happens from here on, the service goes back up.
	defer restart()

	if serr := stopFn(); serr != nil {
		// It may be half stopped: only leave it as found if it still runs.
		if st, e := statusFn(); e == nil && st == "running" {
			once.Do(func() {})
		}
		return fmt.Errorf("could not stop the service: %w", serr)
	}

	// A first signal lets the change in progress finish (cutting a git
	// operation in half is worse than a few seconds); a second restarts the
	// service now and ends the process.
	var mu sync.Mutex
	interrupted := false
	done := make(chan struct{})
	defer close(done)
	go func() {
		n := 0
		for {
			select {
			case <-sigs:
				n++
				mu.Lock()
				interrupted = true
				mu.Unlock()
				if n == 1 {
					fmt.Fprintln(stderr, "interrupted: finishing this change, then restarting the service (press again to restart now)")
					continue
				}
				restart()
				exitFn(130)
				return
			case <-done:
				return
			}
		}
	}()
	wasInterrupted := func() bool { mu.Lock(); defer mu.Unlock(); return interrupted }

	_ = syncpkg.ClearStalePullMarker(syncDir)

	deadline := time.Now().Add(lockWait)
	for {
		l, err = lock.Acquire(syncDir)
		if err == nil {
			break
		}
		if !errors.Is(err, lock.ErrHeld) {
			return err
		}
		if wasInterrupted() {
			return errInterrupted
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: the service was stopped but did not release the lock within %s", lock.ErrHeld, lockWait)
		}
		time.Sleep(lockPoll)
	}
	defer l.Release()

	ferr := fn()
	if ferr == nil && wasInterrupted() {
		return errInterrupted
	}
	return ferr
}
