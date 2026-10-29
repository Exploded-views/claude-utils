package main

import (
	"sync"
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
// func stops the guard and waits for its goroutine, so nothing of the guard
// reads the package seams once it returns. It is idempotent. It cannot hang:
// a goroutine that has begun to exit the process (exitFn may never return, or
// may block in a test) is not waited for, and a signal the sync flow owns is
// swallowed without blocking.
func guardChildren() (stop func()) {
	sigs, stopSigs := notifySignals()
	done := make(chan struct{})
	finished := make(chan struct{}) // closed when the goroutine returns
	exiting := make(chan struct{})  // closed once it has read exitFn and is leaving
	go func() {
		defer close(finished)
		for {
			select {
			case s := <-sigs:
				if syncOwnsSignals.Load() {
					continue // the sync flow decides: the first lets the change finish
				}
				exit := exitFn // read the seam before stop() can return
				close(exiting)
				proc.KillAll()
				code := 130
				if s == syscall.SIGTERM {
					code = 143
				}
				exit(code)
				return
			case <-done:
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { stopSigs(); close(done) })
		select {
		case <-finished:
		case <-exiting:
		}
	}
}
