//go:build !windows

package sync

import (
	"os"
	"path/filepath"
	"testing"
)

type linkHost struct {
	r       Roots
	outside string
	c, m    string // Claude and mirror copies of n.md
}

func newLinkHost(t *testing.T) linkHost {
	t.Helper()
	r, outside := linkRoots(t)
	return linkHost{r: r, outside: outside,
		c: filepath.Join(r.Claude, "h1", "memory", "n.md"),
		m: filepath.Join(r.Mirror, "h1", "memory", "n.md")}
}

func (h linkHost) link(t *testing.T, at string) {
	t.Helper()
	os.WriteFile(filepath.Join(h.outside, "shared.md"), []byte("the user's shared note\n"), 0o600)
	if err := os.Symlink(filepath.Join(h.outside, "shared.md"), at); err != nil {
		t.Skip(err)
	}
}

func isLink(p string) bool {
	st, err := os.Lstat(p)
	return err == nil && st.Mode()&os.ModeSymlink != 0
}

const relN = "h1/memory/n.md"

// A note that exists but is not a regular file, on either side, is skipped:
// not missing (so no deletion goes into the mirror and on to every PC), not
// overwritten, and its manifest entry is untouched.
func TestReconcile_ALinkedNoteIsSkippedNotTreatedAsMissing(t *testing.T) {
	t.Run("claude link, in manifest, mirror copy: no deletion", func(t *testing.T) {
		h := newLinkHost(t)
		os.WriteFile(h.m, []byte("mirror copy\n"), 0o600)
		h.link(t, h.c)
		man := NewManifest()
		man.Add(relN)
		rep, err := Reconcile(h.r, man)
		if err != nil {
			t.Fatal(err)
		}
		if len(rep.RemovedFromMirror) != 0 {
			t.Errorf("a linked note read as deleted: %v", rep.RemovedFromMirror)
		}
		if b, _ := os.ReadFile(h.m); string(b) != "mirror copy\n" {
			t.Errorf("mirror copy changed: %q", b)
		}
		if !isLink(h.c) {
			t.Error("the user's link was replaced")
		}
		if !man.Has(relN) {
			t.Error("the manifest entry was dropped")
		}
	})
	t.Run("claude link, not in manifest: the link is kept", func(t *testing.T) {
		h := newLinkHost(t)
		os.WriteFile(h.m, []byte("mirror copy\n"), 0o600)
		h.link(t, h.c)
		if _, err := Reconcile(h.r, NewManifest()); err != nil {
			t.Fatal(err)
		}
		if !isLink(h.c) {
			t.Error("the mirror copy was written over the user's link")
		}
		if b, _ := os.ReadFile(filepath.Join(h.outside, "shared.md")); string(b) != "the user's shared note\n" {
			t.Errorf("the link target was written: %q", b)
		}
	})
	t.Run("mirror link, in manifest, claude copy: not deleted, not copied", func(t *testing.T) {
		h := newLinkHost(t)
		os.WriteFile(h.c, []byte("claude copy\n"), 0o600)
		h.link(t, h.m)
		man := NewManifest()
		man.Add(relN)
		rep, _ := Reconcile(h.r, man)
		if len(rep.RemovedFromMirror) != 0 || len(rep.CopiedToMirror) != 0 {
			t.Errorf("report: %+v", rep)
		}
		if !isLink(h.m) {
			t.Error("the mirror link was replaced or removed")
		}
		if b, _ := os.ReadFile(h.c); string(b) != "claude copy\n" {
			t.Errorf("claude copy changed: %q", b)
		}
	})
	t.Run("mirror link, claude missing, in manifest: not removed", func(t *testing.T) {
		h := newLinkHost(t)
		h.link(t, h.m)
		man := NewManifest()
		man.Add(relN)
		rep, _ := Reconcile(h.r, man)
		if len(rep.RemovedFromMirror) != 0 || !isLink(h.m) {
			t.Errorf("a linked mirror entry was treated as the user's delete: %+v", rep)
		}
	})
}

// The manifest keeps the name of a note that is a link on the Claude side, so
// removing the link later is still seen as the user's delete.
func TestManifest_KeepsTheNameOfALinkedNote(t *testing.T) {
	h := newLinkHost(t)
	h.link(t, h.c)
	m, err := BuildFromClaudeTree(h.r.Claude)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Has(relN) {
		t.Error("the linked note dropped out of the manifest")
	}
}

// The watcher path: a delete event while the Claude side holds a link, and a
// copy of a link, both leave everything alone.
func TestWatcherHelpers_LeaveALinkedNoteAlone(t *testing.T) {
	h := newLinkHost(t)
	os.WriteFile(h.m, []byte("mirror copy\n"), 0o600)
	h.link(t, h.c)
	if err := RemoveFromMirror(h.r, "h1", "n.md"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(h.m); string(b) != "mirror copy\n" {
		t.Error("RemoveFromMirror deleted the mirror copy of a linked note")
	}
	if err := CopyToMirror(h.r, "h1", "n.md"); err != nil {
		t.Errorf("CopyToMirror of a linked note should skip, got %v", err)
	}
	if b, _ := os.ReadFile(h.m); string(b) != "mirror copy\n" {
		t.Error("CopyToMirror read through the link")
	}
	if err := CopyToClaude(h.r, "h1", "n.md"); err != nil || !isLink(h.c) {
		t.Errorf("CopyToClaude overwrote the user's link: %v", err)
	}
}

// A read-only host's populate skips links and linked folders and carries
// on with the other notes.
func TestPopulateClaude_SkipsLinksAndCarriesOn(t *testing.T) {
	h := newLinkHost(t)
	os.WriteFile(filepath.Join(h.r.Mirror, "h1", "memory", "a-link.md"), nil, 0o600)
	os.Remove(filepath.Join(h.r.Mirror, "h1", "memory", "a-link.md"))
	os.Symlink(filepath.Join(h.outside, "secret.txt"), filepath.Join(h.r.Mirror, "h1", "memory", "a-link.md"))
	os.WriteFile(filepath.Join(h.r.Mirror, "h1", "memory", "b.md"), []byte("b\n"), 0o600)
	os.MkdirAll(filepath.Join(h.r.Mirror, "h2"), 0o700)
	os.Symlink(h.outside, filepath.Join(h.r.Mirror, "h2", "memory"))
	os.WriteFile(filepath.Join(h.outside, "note.md"), []byte(linkSecret), 0o600)
	n, err := PopulateClaude(h.r)
	if err != nil {
		t.Fatalf("one link must not abort the run: %v", err)
	}
	if n != 1 {
		t.Errorf("copied %d, want 1", n)
	}
	if b, _ := os.ReadFile(filepath.Join(h.r.Claude, "h1", "memory", "b.md")); string(b) != "b\n" {
		t.Error("the regular note was not copied")
	}
	noLeak(t, h.r.Claude)
}
