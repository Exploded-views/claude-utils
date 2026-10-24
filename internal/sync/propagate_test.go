package sync

import (
	"strings"
	"testing"
)

// propagateChanges must read git's NUL-separated output: a name with a space
// or a non-ASCII character was split or quoted by the line-based parser.
func TestPropagate_UnusualNames(t *testing.T) {
	body := strings.Repeat("a line long enough for rename detection\n", 8)
	f := newPullFixture(t, map[string]string{
		"old name.md": body,
		"gone é.md":   "bye\n",
		"keep.md":     "k\n",
	})
	captureLog(t)
	f.peerPush("new note.md", "spaced\n")
	f.peerPush("café.md", "accent\n")
	f.peerPush("日本語.md", "cjk\n")
	f.peerPush("gone é.md", "")
	f.peerRename("old name.md", "renamed é.md")

	if err := f.loop.flush(f.repo, f.roots, true); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"new note.md":  "spaced\n",
		"café.md":      "accent\n",
		"日本語.md":      "cjk\n",
		"renamed é.md": body,
	} {
		if got, ok := f.claudeCopy(name); !ok || got != want {
			t.Errorf("%q: got %q, ok=%v", name, got, ok)
		}
	}
	for _, name := range []string{"gone é.md", "old name.md"} {
		if _, ok := f.claudeCopy(name); ok {
			t.Errorf("%q should have been removed from the Claude tree", name)
		}
	}
	if _, ok := f.claudeCopy("keep.md"); !ok {
		t.Error("keep.md vanished")
	}
}
