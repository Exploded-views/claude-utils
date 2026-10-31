//go:build !windows

package sync

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fsnotify/fsnotify"
)

func linkRoots(t *testing.T) (Roots, string) {
	t.Helper()
	d := t.TempDir()
	outside := filepath.Join(d, "outside")
	os.MkdirAll(outside, 0o700)
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte(linkSecret), 0o600)
	r := Roots{Claude: filepath.Join(d, "claude"), Mirror: filepath.Join(d, "mirror")}
	os.MkdirAll(filepath.Join(r.Claude, "h1", "memory"), 0o700)
	os.MkdirAll(filepath.Join(r.Mirror, "h1", "memory"), 0o700)
	return r, outside
}

func noLeak(t *testing.T, root string) {
	t.Helper()
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), linkSecret) {
				t.Errorf("content of a link target reached %s", p)
			}
		}
		return nil
	})
}

// Reconcile never follows a link, on either side and in either direction.
func TestReconcile_SkipsLinksInBothDirections(t *testing.T) {
	cases := map[string]func(r Roots, outside string){
		"mirror note link": func(r Roots, o string) {
			os.Symlink(filepath.Join(o, "secret.txt"), filepath.Join(r.Mirror, "h1", "memory", "note.md"))
		},
		"claude note link": func(r Roots, o string) {
			os.Symlink(filepath.Join(o, "secret.txt"), filepath.Join(r.Claude, "h1", "memory", "note.md"))
		},
		"mirror memory dir link": func(r Roots, o string) {
			os.Remove(filepath.Join(r.Mirror, "h1", "memory"))
			os.Symlink(o, filepath.Join(r.Mirror, "h1", "memory"))
		},
		"claude memory dir link": func(r Roots, o string) {
			os.Remove(filepath.Join(r.Claude, "h1", "memory"))
			os.Symlink(o, filepath.Join(r.Claude, "h1", "memory"))
		},
	}
	for name, plant := range cases {
		t.Run(name, func(t *testing.T) {
			r, outside := linkRoots(t)
			os.WriteFile(filepath.Join(outside, "note.md"), []byte(linkSecret), 0o600)
			plant(r, outside)
			before, _ := os.ReadFile(filepath.Join(outside, "secret.txt"))
			if _, err := Reconcile(r, NewManifest()); err != nil {
				t.Logf("reconcile: %v", err)
			}
			noLeak(t, r.Claude)
			noLeak(t, r.Mirror)
			if after, _ := os.ReadFile(filepath.Join(outside, "secret.txt")); string(after) != string(before) {
				t.Error("the target was written through the link")
			}
			if _, err := os.Stat(filepath.Join(outside, "MEMORY.md")); err == nil {
				t.Error("a file was created in the link target")
			}
		})
	}
}

func TestCopyHelpers_RefuseLinks(t *testing.T) {
	r, outside := linkRoots(t)
	os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(r.Mirror, "h1", "memory", "n.md"))
	os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(r.Claude, "h1", "memory", "n.md"))
	// A link is skipped, not an error that stops a cycle, and never followed.
	if err := CopyToClaude(r, "h1", "n.md"); err != nil {
		t.Error(err)
	}
	if err := CopyToMirror(r, "h1", "n.md"); err != nil {
		t.Error(err)
	}
	if ok, b := readIfExists(filepath.Join(r.Mirror, "h1", "memory", "n.md")); ok || b != nil {
		t.Error("readIfExists followed a link")
	}
	noLeak(t, r.Claude)
	noLeak(t, r.Mirror)
}

func TestCopyHelpers_RefuseALinkedFolder(t *testing.T) {
	r, outside := linkRoots(t)
	os.WriteFile(filepath.Join(outside, "n.md"), []byte("x"), 0o600)
	os.Remove(filepath.Join(r.Mirror, "h1", "memory"))
	os.Symlink(outside, filepath.Join(r.Mirror, "h1", "memory"))
	if err := CopyToClaude(r, "h1", "n.md"); err != nil {
		t.Error(err)
	}
	if err := CopyToMirror(r, "h1", "n.md"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Error(err)
	}
	if _, err := os.Stat(filepath.Join(r.Claude, "h1", "memory", "n.md")); err == nil {
		t.Error("CopyToClaude read through a linked memory folder")
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "n.md")); string(b) != "x" {
		t.Error("CopyToMirror wrote through a linked memory folder")
	}
}

func TestManifestAndWatches_SkipLinks(t *testing.T) {
	r, outside := linkRoots(t)
	os.WriteFile(filepath.Join(outside, "n.md"), []byte("x"), 0o600)
	os.Remove(filepath.Join(r.Claude, "h1", "memory"))
	os.Symlink(outside, filepath.Join(r.Claude, "h1", "memory"))
	m, err := BuildFromClaudeTree(r.Claude)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.files) != 0 {
		t.Errorf("the manifest followed a linked memory folder: %v", m.files)
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		t.Skip(err)
	}
	defer w.Close()
	if err := refreshMemoryWatches(w, r.Claude); err != nil {
		t.Fatal(err)
	}
	if n := len(w.WatchList()); n != 0 {
		t.Errorf("a linked memory folder is watched: %v", w.WatchList())
	}
}
