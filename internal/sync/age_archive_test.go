package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fsnotify/fsnotify"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
)

// Archive-deletes on top of age: the merge-time interactions between the two.

// ageArchiveHosts returns two encrypted hosts with archiving on that both hold
// the seed notes.
func ageArchiveHosts(t *testing.T, seed map[string]string) (a, b *ageHost) {
	t.Helper()
	archClock(t)
	captureLog(t)
	origin := ageEnv(t)
	a = newAgeHost(t, origin, "aaa", true)
	for n, c := range seed {
		a.edit(n, c)
	}
	if err := a.flushRetry(); err != nil {
		t.Fatal(err)
	}
	b = newAgeHost(t, origin, "bbb", false)
	b.join(a)
	b.loop.pullTick(b.repo, b.roots) // the notes that were ciphertext before admission
	a.loop.Cfg.ArchiveDeletes = true
	b.loop.Cfg.ArchiveDeletes = true
	for n := range seed {
		if b.claude(n) == "" {
			t.Fatalf("setup: %s did not reach host b\n%s", n, mustGit(t, b.repo, "ls-files"))
		}
	}
	return a, b
}

func (h *ageHost) userDeletes(name string) {
	h.t.Helper()
	p := filepath.Join(h.roots.Claude, "p1", "memory", name)
	if err := os.Remove(p); err != nil {
		h.t.Fatal(err)
	}
	handled, err := h.loop.handleEvent(nil, h.roots, fsnotify.Event{Name: p, Op: fsnotify.Remove})
	if err != nil || !handled {
		h.t.Fatalf("remove event: handled=%v err=%v", handled, err)
	}
}

func (h *ageHost) clean() {
	h.t.Helper()
	if h.repo.RebaseInProgress() {
		h.t.Fatal("rebase left in progress")
	}
	if d, _ := h.repo.Run("status", "--porcelain"); strings.TrimSpace(d) != "" {
		h.t.Fatalf("dirty clone:\n%s", d)
	}
}

// treeHas reports whether the committed tree holds a path with this prefix.
func (h *ageHost) treeHas(prefix string) bool {
	out, _ := h.repo.Run("ls-tree", "-r", "--name-only", "HEAD")
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

// The index's union rule lives in .gitattributes, but age's info/attributes
// outranks it. Without a rule of its own two hosts that archive in the same
// cycle conflict on archive/INDEX.md and every pull aborts.
func TestAgeArchive_TwoHostsAppendingIndexMergeAsUnion(t *testing.T) {
	a, b := ageArchiveHosts(t, map[string]string{"x.md": "xx\n", "y.md": "yy\n", "keep.md": "k\n"})
	a.userDeletes("x.md")
	b.userDeletes("y.md")
	if err := a.flushRetry(); err != nil {
		t.Fatal(err)
	}
	if err := b.flushRetry(); err != nil {
		t.Fatalf("b's pull must not abort on the index: %v", err)
	}
	if err := a.flushRetry(); err != nil {
		t.Fatal(err)
	}
	for _, h := range []*ageHost{a, b} {
		h.clean()
		idx, err := os.ReadFile(filepath.Join(h.repo.Dir, "archive", "INDEX.md"))
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range []string{"x.md", "y.md"} {
			if !strings.Contains(string(idx), w) {
				t.Errorf("%s: INDEX.md lost %s:\n%s", h.loop.Hostname, w, idx)
			}
		}
		if strings.Contains(string(idx), "<<<<<<<") {
			t.Errorf("%s: conflict markers in INDEX.md", h.loop.Hostname)
		}
	}
	blob := mustGit(t, a.repo, "cat-file", "blob", "HEAD:archive/INDEX.md")
	if !agecrypt.IsCiphertext([]byte(blob)) {
		t.Error("INDEX.md must be stored encrypted")
	}
	if b.claude("x.md") != "" || a.claude("y.md") != "" {
		t.Error("a deleted note came back")
	}
	if b.claude("keep.md") == "" {
		t.Error("lost an unrelated note")
	}
}

// archivedText finds the archive copy of a note in the work-tree and returns its text.
func (h *ageHost) archivedText(name string) (string, bool) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	var got string
	var found bool
	filepath.Walk(filepath.Join(h.repo.Dir, "archive"), func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && strings.HasPrefix(fi.Name(), stem+".") && strings.HasSuffix(fi.Name(), ext) && fi.Name() != "INDEX.md" {
			b, _ := os.ReadFile(p)
			got, found = string(b), true
		}
		return nil
	})
	return got, found
}

func (h *ageHost) settle(others ...*ageHost) {
	h.t.Helper()
	for i := 0; i < 2; i++ {
		for _, x := range append([]*ageHost{h}, others...) {
			if err := x.flushRetry(); err != nil {
				h.t.Fatalf("%s: %v", x.loop.Hostname, err)
			}
			x.loop.pullTick(x.repo, x.roots)
		}
	}
}

// Under age a moved note never pairs as a rename (every blob is ciphertext), so
// an edit on one host and the archive of the same note on another is a
// modify/delete conflict. The archive resolver must settle it, keeping both
// versions, instead of aborting every cycle.
func TestAgeArchive_EditVsArchive_KeepsBothAndDoesNotWedge(t *testing.T) {
	for _, archiverPushesFirst := range []bool{true, false} {
		a, b := ageArchiveHosts(t, map[string]string{"n.md": "original\n", "keep.md": "k\n"})
		a.userDeletes("n.md")
		b.edit("n.md", "edited on b\n")
		first, second := a, b
		if !archiverPushesFirst {
			first, second = b, a
		}
		if err := first.flushRetry(); err != nil {
			t.Fatal(err)
		}
		if err := second.flushRetry(); err != nil {
			t.Fatalf("archiverPushesFirst=%v: the second host's pull must resolve, not abort: %v", archiverPushesFirst, err)
		}
		a.settle(b)
		for _, h := range []*ageHost{a, b} {
			h.clean()
			if got := h.claude("n.md"); got != "edited on b\n" {
				t.Errorf("%s: the edit must survive live, got %q", h.loop.Hostname, got)
			}
			if got, ok := h.archivedText("n.md"); !ok || got != "original\n" {
				t.Errorf("%s: the archive copy must keep the original, got %q %v", h.loop.Hostname, got, ok)
			}
			if h.claude("keep.md") != "k\n" {
				t.Errorf("%s: lost an unrelated note", h.loop.Hostname)
			}
		}
		// And it stays settled: another cycle has nothing to abort.
		if err := a.flushRetry(); err != nil {
			t.Errorf("not settled: %v", err)
		}
	}
}

// A real move is an archive plus an add under age (no rename pairing). Nothing
// is lost: the new name is live everywhere and the old text sits in the archive.
func TestAgeArchive_RealRenameIsArchivePlusAdd(t *testing.T) {
	a, b := ageArchiveHosts(t, map[string]string{"old.md": "moved text\n", "keep.md": "k\n"})
	a.userDeletes("old.md")
	a.edit("new.md", "moved text\n")
	a.settle(b)
	for _, h := range []*ageHost{a, b} {
		h.clean()
		if h.claude("new.md") != "moved text\n" || h.claude("old.md") != "" {
			t.Errorf("%s: new=%q old=%q", h.loop.Hostname, h.claude("new.md"), h.claude("old.md"))
		}
		if got, ok := h.archivedText("old.md"); !ok || got != "moved text\n" {
			t.Errorf("%s: archive copy of the old name: %q %v", h.loop.Hostname, got, ok)
		}
		// The copy's name carries the hash of the plaintext, never of the
		// ciphertext stored in git, and no cycle "repaired" it.
		if !h.treeHas(archPath("p1", "old.md", "moved text\n")) {
			t.Errorf("%s: archive copy is not named for the plaintext hash:\n%s", h.loop.Hostname, headTree(t, h.repo))
		}
		if log := mustGit(t, h.repo, "log", "--format=%s"); strings.Contains(log, "restore edited notes") {
			t.Errorf("%s: a clean archive was 'restored':\n%s", h.loop.Hostname, log)
		}
	}
}
