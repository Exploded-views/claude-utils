package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fsnotify/fsnotify"
)

func only(folders ...string) func(string) bool {
	return func(f string) bool {
		for _, o := range folders {
			if f == o {
				return true
			}
		}
		return false
	}
}

// twoRepos builds two sync repos (each with its own origin and peer) that share
// one Claude tree: A is the client repo and owns folder "pc", B owns "po".
func twoRepos(t *testing.T) (a, b *pullFixture) {
	t.Helper()
	a = newPullFixture(t, map[string]string{})
	b = newPullFixture(t, map[string]string{})
	b.roots.Claude = a.roots.Claude
	a.roots.Owns = only("pc")
	b.roots.Owns = only("po")
	return a, b
}

func repoFiles(t *testing.T, f *pullFixture) string {
	t.Helper()
	return mustGit(t, f.repo, "ls-files")
}

// A client file arriving through repo A's pull is written into the Claude tree.
// Repo B's watcher then sees it appear, and its periodic Reconcile walks it:
// neither may mirror it into B.
func TestMultiRepo_ClientFileNeverReachesOtherRepo(t *testing.T) {
	a, b := twoRepos(t)
	captureLog(t)

	a.peerWrite("projects/pc/memory/secret.md", "client note\n")
	if err := a.loop.flush(a.repo, a.roots, true); err != nil {
		t.Fatal(err)
	}
	claudeFile := filepath.Join(a.roots.Claude, "pc", "memory", "secret.md")
	if _, err := os.Stat(claudeFile); err != nil {
		t.Fatalf("repo A did not propagate its own folder: %v", err)
	}

	// The watcher path.
	handled, err := b.loop.handleEvent(nil, b.roots, fsnotify.Event{Name: claudeFile, Op: fsnotify.Create})
	if err != nil || handled {
		t.Fatalf("B's watcher took a folder it does not own: handled=%v err=%v", handled, err)
	}
	handled, err = b.loop.handleEvent(nil, b.roots, fsnotify.Event{Name: claudeFile, Op: fsnotify.Write})
	if err != nil || handled {
		t.Fatalf("B's watcher took a write: handled=%v err=%v", handled, err)
	}
	// The periodic Reconcile path, then a full cycle.
	if _, err := Reconcile(b.roots, NewManifest()); err != nil {
		t.Fatal(err)
	}
	if err := b.loop.flush(b.repo, b.roots, true); err != nil {
		t.Fatal(err)
	}
	if got := repoFiles(t, b); strings.Contains(got, "pc/") || strings.Contains(got, "secret") {
		t.Fatalf("client file reached repo B: %q", got)
	}
	if _, err := os.Stat(filepath.Join(b.roots.Mirror, "pc")); err == nil {
		t.Fatal("repo B's mirror holds the client folder")
	}
}

// The same flow without the gate leaks, so the test above proves something.
func TestMultiRepo_ControlWithoutGateLeaks(t *testing.T) {
	a, b := twoRepos(t)
	b.roots.Owns = nil
	captureLog(t)
	a.peerWrite("projects/pc/memory/secret.md", "client note\n")
	if err := a.loop.flush(a.repo, a.roots, true); err != nil {
		t.Fatal(err)
	}
	if _, err := Reconcile(b.roots, NewManifest()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(b.roots.Mirror, "pc", "memory", "secret.md")); err != nil {
		t.Fatalf("control: expected the ungated repo to mirror the file: %v", err)
	}
}

func TestMultiRepo_EachRepoMirrorsOnlyItsFolders(t *testing.T) {
	a, b := twoRepos(t)
	captureLog(t)
	writeMem(t, a.roots.Claude, "pc", "c.md", "client\n")
	writeMem(t, a.roots.Claude, "po", "o.md", "own\n")
	writeMem(t, a.roots.Claude, "px", "x.md", "unrouted\n") // owned by nobody
	for _, f := range []*pullFixture{a, b} {
		if _, err := Reconcile(f.roots, NewManifest()); err != nil {
			t.Fatal(err)
		}
		if err := f.loop.flush(f.repo, f.roots, true); err != nil {
			t.Fatal(err)
		}
	}
	fa, fb := repoFiles(t, a), repoFiles(t, b)
	if !strings.Contains(fa, "projects/pc/memory/c.md") || strings.Contains(fa, "po/") || strings.Contains(fa, "px/") {
		t.Errorf("repo A: %q", fa)
	}
	if !strings.Contains(fb, "projects/po/memory/o.md") || strings.Contains(fb, "pc/") || strings.Contains(fb, "px/") {
		t.Errorf("repo B: %q", fb)
	}
}

// Files at a foreign path inside a repo (a frozen copy of a folder that moved
// away) are not copied into the Claude tree by a pull or a catch-up.
func TestMultiRepo_PropagateSkipsUnownedPaths(t *testing.T) {
	_, b := twoRepos(t)
	captureLog(t)
	b.peerWrite("projects/pc/memory/old.md", "frozen\n")
	b.peerWrite("projects/po/memory/new.md", "mine\n")
	if err := b.loop.flush(b.repo, b.roots, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(b.roots.Claude, "po", "memory", "new.md")); err != nil {
		t.Fatalf("owned folder not propagated: %v", err)
	}
	if _, err := os.Stat(filepath.Join(b.roots.Claude, "pc", "memory", "old.md")); err == nil {
		t.Fatal("unowned folder was propagated into the Claude tree")
	}
}

func TestMultiRepo_ManifestOmitsUnownedFolders(t *testing.T) {
	root := t.TempDir()
	writeMem(t, root, "pc", "c.md", "x")
	writeMem(t, root, "po", "o.md", "y")
	m, err := BuildFromClaudeTreeOwned(root, only("po"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Has("pc/memory/c.md") || !m.Has("po/memory/o.md") {
		t.Fatalf("manifest len %d", m.Len())
	}
	// nil gate = today's behaviour
	all, _ := BuildFromClaudeTreeOwned(root, nil)
	if all.Len() != 2 {
		t.Fatalf("nil gate should list all, got %d", all.Len())
	}
}

// A repo that owns no folders (personal) syncs its work-tree and nothing else.
func TestMultiRepo_RepoOwningNothingIgnoresClaudeTree(t *testing.T) {
	a, _ := twoRepos(t)
	captureLog(t)
	a.roots.Owns = func(string) bool { return false }
	writeMem(t, a.roots.Claude, "pc", "c.md", "client\n")
	if _, err := Reconcile(a.roots, NewManifest()); err != nil {
		t.Fatal(err)
	}
	writeRepoFile(t, a.repo, "distilled/note.md", "personal\n")
	if err := a.loop.flush(a.repo, a.roots, true); err != nil {
		t.Fatal(err)
	}
	files := repoFiles(t, a)
	if strings.Contains(files, "projects/") || !strings.Contains(files, "distilled/note.md") {
		t.Fatalf("files: %q", files)
	}
}

// The tests below call the gated functions directly, so each fails when its
// own Owns check is removed.

func TestOwnsGate_RefreshMirrorSkipsUnownedPath(t *testing.T) {
	roots := Roots{Claude: filepath.Join(t.TempDir(), "c"), Mirror: filepath.Join(t.TempDir(), "m"), Owns: only("po")}
	writeMem(t, roots.Claude, "pc", "x.md", "client\n")
	writeMem(t, roots.Claude, "po", "y.md", "own\n")
	refreshMirror(roots, []parkedPath{
		{Path: "projects/pc/memory/x.md", Kind: parkFile},
		{Path: "projects/po/memory/y.md", Kind: parkFile},
	})
	if _, err := os.Stat(filepath.Join(roots.Mirror, "pc", "memory", "x.md")); err == nil {
		t.Fatal("refreshMirror copied an unowned folder into the mirror")
	}
	if _, err := os.Stat(filepath.Join(roots.Mirror, "po", "memory", "y.md")); err != nil {
		t.Fatalf("owned folder not refreshed: %v", err)
	}
}

func TestOwnsGate_KeepIncomingDoesNotMergeIntoUnownedClaudeCopy(t *testing.T) {
	f := newPullFixture(t, map[string]string{})
	rel := "projects/pc/memory/MEMORY.md"
	base := "# Index\n- a\n- b\n- c\n- d\n- e\n- f\n"
	f.peerWrite(rel, base)
	mustGit(t, f.repo, "pull", "-q", "origin", "main")
	pre := strings.TrimSpace(mustGit(t, f.repo, "rev-parse", "HEAD"))
	f.peerWrite(rel, base+"- incoming\n")
	mustGit(t, f.repo, "pull", "-q", "origin", "main")

	saved := "- mine\n" + base
	writeMem(t, f.roots.Claude, "pc", "MEMORY.md", saved)
	for _, c := range []struct {
		name   string
		owns   func(string) bool
		merged bool
	}{{"control (no gate)", nil, true}, {"unowned", only("po"), false}} {
		writeMem(t, f.roots.Claude, "pc", "MEMORY.md", saved)
		roots := f.roots
		roots.Owns = c.owns
		if _, _, err := keepIncoming(f.repo, roots, pre, rel, []byte(saved), true); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(filepath.Join(f.roots.Claude, "pc", "memory", "MEMORY.md"))
		if got := string(b) != saved; got != c.merged {
			t.Errorf("%s: Claude copy changed=%v, want %v", c.name, got, c.merged)
		}
	}
}

func TestOwnsGate_CatchUpSkipsKeepsNothingForUnownedPath(t *testing.T) {
	f := newPullFixture(t, map[string]string{})
	rec := strings.TrimSpace(mustGit(t, f.repo, "rev-parse", "HEAD"))
	f.peerWrite("projects/pc/memory/k.md", "incoming\n")
	mustGit(t, f.repo, "pull", "-q", "origin", "main")
	head := strings.TrimSpace(mustGit(t, f.repo, "rev-parse", "HEAD"))
	writeMem(t, f.roots.Claude, "pc", "k.md", "local edit\n")

	for _, c := range []struct {
		owns func(string) bool
		keep int
	}{{nil, 1}, {only("po"), 0}} {
		roots := f.roots
		roots.Owns = c.owns
		_, keep, err := catchUpSkips(f.repo, roots, rec, head)
		if err != nil {
			t.Fatal(err)
		}
		if len(keep) != c.keep {
			t.Errorf("owns=%v: kept %d copies, want %d", c.owns != nil, len(keep), c.keep)
		}
	}
}

// A rename from a folder this repo does not own into one it owns removes
// nothing from the unowned folder's Claude copy.
func TestOwnsGate_RenameOldPathInUnownedFolderLeftAlone(t *testing.T) {
	f := newPullFixture(t, map[string]string{})
	captureLog(t)
	body := strings.Repeat("a line long enough for rename detection\n", 8)
	f.peerWrite("projects/pc/memory/a.md", body)
	mustGit(t, f.repo, "pull", "-q", "origin", "main")
	f.roots.Owns = only("po")
	writeMem(t, f.roots.Claude, "pc", "a.md", body)

	mustGit(t, f.peer, "pull", "-q", "origin", "main")
	if err := os.MkdirAll(filepath.Join(f.peer.Dir, "projects", "po", "memory"), 0700); err != nil {
		t.Fatal(err)
	}
	mustGit(t, f.peer, "mv", "projects/pc/memory/a.md", "projects/po/memory/a.md")
	mustGit(t, f.peer, "commit", "-q", "-m", "move")
	mustGit(t, f.peer, "push", "-q", "origin", "main")
	if err := f.loop.flush(f.repo, f.roots, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.roots.Claude, "po", "memory", "a.md")); err != nil {
		t.Fatalf("owned rename target not propagated: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.roots.Claude, "pc", "memory", "a.md")); err != nil {
		t.Fatal("the rename's old path in an unowned folder was deleted from the Claude tree")
	}
}
