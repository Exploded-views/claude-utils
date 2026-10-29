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
// the children are killed, and the flow decides when to exit. The returned
// func stops the guard.
func guardChildren() (stop func()) {
	sigs, stopSigs := notifySignals()
	done := make(chan struct{})
	go func() {
		select {
		case s := <-sigs:
			proc.KillAll()
			if syncOwnsSignals.Load() {
				return
			}
			code := 130
			if s == syscall.SIGTERM {
				code = 143
			}
			exitFn(code)
		case <-done:
		}
	}()
	return func() { stopSigs(); close(done) }
}
