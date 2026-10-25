package sync

import (
	"github.com/fsnotify/fsnotify"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExpiry_HoldsAfterTheDate(t *testing.T) {
	f := newPullFixture(t, map[string]string{"a.md": "a\n"})
	captureLog(t)
	f.loop.Cfg.ExpiresAt = "2026-06-01"
	clock := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	f.loop.Now = func() time.Time { return clock }

	// Before the date the host syncs: a peer's note arrives, a local note goes out.
	f.peerPush("b.md", "before expiry\n")
	writeMem(t, f.roots.Claude, "p1", "mine.md", "local note\n")
	f.loop.pullTick(f.repo, f.roots)
	if got, ok := f.claudeCopy("b.md"); !ok || got != "before expiry\n" {
		t.Fatalf("before the date the host must sync: %q %v", got, ok)
	}
	if !f.originHas("mine.md") {
		t.Fatal("before the date the host must push")
	}

	// The clock passes the date: a hold, like a foreign-writer hold.
	clock = time.Date(2026, 6, 2, 0, 0, 1, 0, time.UTC)
	originBefore, headBefore := f.originHead(), f.head()
	f.peerPush("c.md", "after expiry\n")
	writeMem(t, f.roots.Claude, "p1", "later.md", "written after expiry\n")
	f.loop.pullTick(f.repo, f.roots)
	if _, ok := f.claudeCopy("c.md"); ok {
		t.Error("an expired host must not pull notes into the Claude tree")
	}
	if f.head() != headBefore {
		t.Error("an expired host must not pull")
	}
	if f.originHas("later.md") {
		t.Error("an expired host must not push")
	}
	_ = originBefore
	if n := strings.Count(f.autoCommitsOnOrigin(), "auto:"); n > 1 {
		t.Errorf("an expired host must not commit: %d auto commits on origin", n)
	}
	if handled, _ := f.loop.handleEvent(nil, f.roots, fsEvent(filepath.Join(f.roots.Claude, "p1", "memory", "later.md"))); handled {
		t.Error("an expired host ignores file events")
	}
	if !f.loop.Expired() {
		t.Error("Expired() should report the hold for status and logs")
	}
}

func TestExpiry_NoDateMeansNoHold(t *testing.T) {
	f := newPullFixture(t, map[string]string{"a.md": "a\n"})
	captureLog(t)
	f.loop.Now = func() time.Time { return time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC) }
	f.peerPush("b.md", "x\n")
	f.loop.pullTick(f.repo, f.roots)
	if _, ok := f.claudeCopy("b.md"); !ok {
		t.Error("without expiresAt the host syncs whatever the clock says")
	}
}

func fsEvent(name string) fsnotify.Event { return fsnotify.Event{Name: name, Op: fsnotify.Write} }
