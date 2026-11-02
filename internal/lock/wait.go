package lock

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// ErrTimeout means WaitFile gave up before the lock came free.
var ErrTimeout = errors.New("timed out waiting for the file lock")

// waitPoll is how often WaitFile retries a held lock.
const waitPoll = 5 * time.Millisecond

// WaitFile takes an exclusive advisory lock on f, waiting up to timeout for
// another holder to let go. It returns a func that drops the lock; the caller
// still owns (and closes) f. A timeout wraps ErrTimeout. The lock is released
// by the OS if the process dies.
func WaitFile(f *os.File, timeout time.Duration) (release func(), err error) {
	deadline := time.Now().Add(timeout)
	for {
		err := tryLock(f)
		if err == nil {
			return func() { unlock(f) }, nil
		}
		if !errors.Is(err, errWouldBlock) {
			return nil, err
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("%w (%v)", ErrTimeout, timeout)
		}
		time.Sleep(waitPoll)
	}
}
