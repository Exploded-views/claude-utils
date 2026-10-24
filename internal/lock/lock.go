// Package lock keeps a second daemon off a sync dir. The lock is an advisory
// file lock held for the life of the process, so a crashed daemon never leaves
// a stale lock behind.
package lock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrHeld means another daemon already holds the sync dir.
var ErrHeld = errors.New("another claude-memsync daemon already runs on this sync dir")

// Lock is a held sync-dir lock.
type Lock struct{ f *os.File }

// Path is the lock file of a sync dir. It sits in .state, which is git-ignored.
func Path(syncDir string) string { return filepath.Join(syncDir, ".state", "daemon.lock") }

// Acquire takes the sync dir's lock without waiting.
func Acquire(syncDir string) (*Lock, error) {
	p := Path(syncDir)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := tryLock(f); err != nil {
		f.Close()
		if errors.Is(err, errWouldBlock) {
			return nil, fmt.Errorf("%w (%s)", ErrHeld, syncDir)
		}
		return nil, err
	}
	_ = f.Truncate(0)
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	return &Lock{f: f}, nil
}

// Release drops the lock.
func (l *Lock) Release() {
	if l == nil || l.f == nil {
		return
	}
	unlock(l.f)
	l.f.Close()
	l.f = nil
}
