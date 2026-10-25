package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fsnotify/fsnotify"
)

// While Hold says so, the loop mirrors, commits and pushes nothing; when it
// lets go, the next cycle carries the folder.
func TestLoop_HoldPausesMirrorCommitAndPush(t *testing.T) {
	f := newPullFixture(t, map[string]string{})
	captureLog(t)
	held := true
	f.loop.Hold = func() bool { return held }
	p := filepath.Join(f.roots.Claude, "p9", "memory", "n.md")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("note\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := f.head()

	handled, err := f.loop.handleEvent(nil, f.roots, fsnotify.Event{Name: p, Op: fsnotify.Write})
	if err != nil || handled {
		t.Fatalf("held: handled=%v err=%v", handled, err)
	}
	f.loop.pullTick(f.repo, f.roots)
	if err := f.loop.flush(f.repo, f.roots, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.roots.Mirror, "p9")); err == nil {
		t.Error("held loop mirrored a folder")
	}
	if f.head() != before {
		t.Error("held loop committed")
	}

	held = false
	f.loop.pullTick(f.repo, f.roots)
	if f.head() == before {
		t.Error("released loop did not commit")
	}
}
