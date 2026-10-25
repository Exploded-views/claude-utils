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
