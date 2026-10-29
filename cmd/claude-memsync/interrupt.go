package main

import (
	"sync/atomic"
	"syscall"

	"github.com/MarimerLLC/claude-utils/internal/proc"
)

// syncOwnsSignals is set while withSyncLocksStopping handles SIGINT and
// SIGTERM itself (the first lets the change finish, the second exits).
var syncOwnsSignals atomic.Bool

// guardChildren makes SIGINT and SIGTERM kill the git process groups this
// process started before it exits. A git child leads a group of its own, so the
// terminal's Ctrl-C no longer reaches it and it would run on as an orphan.
// While a command is stopping the service the signals belong to that flow:
// the guard does nothing, and the flow kills the children on its second
// signal, just before it exits. The returned
// func stops the guard.
func guardChildren() (stop func()) {
	sigs, stopSigs := notifySignals()
	done := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-sigs:
				if syncOwnsSignals.Load() {
					continue // the sync flow decides: the first lets the change finish
				}
				proc.KillAll()
				code := 130
				if s == syscall.SIGTERM {
					code = 143
				}
				exitFn(code)
				return
			case <-done:
				return
			}
		}
	}()
	return func() { stopSigs(); close(done) }
}
