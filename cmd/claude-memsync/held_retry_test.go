package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/config"
)

// A held repo is alerted once per session per hold: the retries of one
// failure rewrite loop-failed, but must not change what the session already
// heard. A different failure is a new hold record and alerts again.
func TestAwaitPrivate_RetriesOfOneFailureAlertOncePerSession(t *testing.T) {
	oldMin, oldRelease, oldWait := guardBackoffMin, guardRelease, guardWait
	t.Cleanup(func() { guardBackoffMin, guardRelease, guardWait = oldMin, oldRelease, oldWait })
	cfg := config.Config{SyncDir: t.TempDir()}
	targets := []hookTarget{{name: "x", cfg: cfg}}

	alerts := 0
	hear := func() {
		lines, commit := heldAlerts(targets, "sess-r")
		alerts += len(lines)
		commit()
	}
	waits := 0
	guardWait = func(ctx context.Context, d time.Duration) bool {
		hear()
		waits++
		time.Sleep(1100 * time.Millisecond) // the record's timestamp has 1 s resolution
		return true
	}
	n := 0
	guardRelease = func(config.Config, func(string, ...any)) error {
		n++
		switch {
		case n <= 2:
			return errors.New("installation lookup: GitHub returned HTTP 404")
		case n == 3:
			return errors.New("repo is public")
		}
		return nil
	}
	if !awaitPrivate(context.Background(), "x", cfg, errors.New("installation lookup: GitHub returned HTTP 404")) {
		t.Fatal("awaitPrivate must report true once the check passes")
	}
	// waits: 1st record (A), 2nd (A again), 3rd (A again), 4th (B).
	if waits != 4 {
		t.Fatalf("%d waits, want 4", waits)
	}
	if alerts != 2 {
		t.Errorf("%d alerts over three retries of one failure and one new failure, want 2", alerts)
	}
}
