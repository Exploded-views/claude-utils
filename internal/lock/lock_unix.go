//go:build !windows

package lock

import (
	"errors"
	"os"
	"syscall"
)

var errWouldBlock = errors.New("would block")

func tryLock(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return errWouldBlock
	}
	return err
}

func unlock(f *os.File) { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }

// tryLockWait and unlockWait lock the whole file (flock has no ranges).
func tryLockWait(f *os.File) error { return tryLock(f) }
func unlockWait(f *os.File)        { unlock(f) }
