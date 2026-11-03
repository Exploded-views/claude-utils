package lock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

var errWouldBlock = errors.New("would block")

func tryLock(f *os.File) error {
	ol := new(windows.Overlapped)
	err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return errWouldBlock
	}
	return err
}

func unlock(f *os.File) {
	ol := new(windows.Overlapped)
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
}

// waitOverlapped addresses one byte at offset 0x7FFFFFFF_FFFFFFFE, far past any
// real data, so a WaitFile lock never blocks a reader or writer of the file.
func waitOverlapped() *windows.Overlapped {
	return &windows.Overlapped{Offset: 0xFFFFFFFE, OffsetHigh: 0x7FFFFFFF}
}

func tryLockWait(f *os.File) error {
	err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, waitOverlapped())
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return errWouldBlock
	}
	return err
}

func unlockWait(f *os.File) {
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, waitOverlapped())
}
