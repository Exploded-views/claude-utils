package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// awaitFlush receives the next OnFlush result or fails after a generous bound.
// The bound only matters when the loop is broken; a healthy run never waits on it.
func awaitFlush(t *testing.T, flushes <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-flushes:
		return err
	case <-time.After(30 * time.Second):
		t.Fatalf("no flush: %s", what)
		return nil
	}
}

// The daemon's watcher branches, driven deterministically. Run's event cases
// are otherwise reached only by whatever events the startup work happens to
// leave queued when a test cancels the context, and select picks at random
// between a ready event and a cancelled context.
//
// Handshake: an event the loop ignores is written first, then one it acts on.
// inotify delivers in order, so by the time the acted-on event's debounced
// flush reports, the ignored one has been dispatched too. The mirror copy
// appears before that flush, so each flush is checked for it.
func TestLoop_WatcherEventsDispatchDeterministically(t *testing.T) {
	f := newPullFixture(t, map[string]string{"note.md": "alpha\n"})
	captureLog(t)
	f.loop.Cfg.DebounceMs = 10
	flushes := make(chan error, 64)
	f.loop.OnFlush = func(_ bool, err error) { flushes <- err }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel) // a failing test must not leave the loop running
	done := make(chan error, 1)
	go func() { done <- f.loop.Run(ctx) }()

	awaitFlush(t, flushes, "startup flush") // the watcher is registered before it

	// Unhandled: outside <hash>/memory/, and an ignored dotfile in memory/.
	if err := os.WriteFile(filepath.Join(f.roots.Claude, "stray.txt"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	writeMem(t, f.roots.Claude, "p1", ".hidden", "x")
	// Handled: a new memory file.
	writeMem(t, f.roots.Claude, "p1", "new.md", "fresh\n")

	mirrored := filepath.Join(f.roots.Mirror, "p1", "memory", "new.md")
	deadline := time.Now().Add(time.Minute) // overall bound: flushes may keep coming without the copy
	for {
		if _, err := os.Stat(mirrored); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the new memory file never reached the mirror")
		}
		awaitFlush(t, flushes, "debounced flush after a memory file was written")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
