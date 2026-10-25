package sync

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fsnotify/fsnotify"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

func (f *pullFixture) originHead() string {
	f.t.Helper()
	out, err := f.repo.Run("ls-remote", "origin", "refs/heads/main")
	if err != nil {
		f.t.Fatal(err)
	}
	return strings.Fields(out)[0]
}

func (f *pullFixture) autoCommitsOnOrigin() string {
	f.t.Helper()
	out, _ := gitwt.New(f.origin).Run("log", "--format=%s", "main")
	var autos []string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "auto:") {
			autos = append(autos, l)
		}
	}
	return strings.Join(autos, "\n")
}

func TestReadOnly_PullsAndPropagatesButNeverCommitsOrPushes(t *testing.T) {
	f := newPullFixture(t, map[string]string{"a.md": "a\n"})
	captureLog(t)
	f.loop.Cfg.ReadOnly = true
	originBefore, headBefore := f.originHead(), f.head()

	// Local activity in the Claude tree: a new note and an edit.
	writeMem(t, f.roots.Claude, "p1", "mine.md", "only here\n")
	writeMem(t, f.roots.Claude, "p1", "a.md", "a\nlocal edit\n")
	f.loop.pullTick(f.repo, f.roots)
	if f.head() != headBefore || f.originHead() != originBefore {
		t.Fatal("a read-only host committed or pushed local activity")
	}

	// A peer pushes: the note arrives in the Claude tree.
	f.peerPush("b.md", "from the peer\n")
	f.loop.pullTick(f.repo, f.roots)
	if got, ok := f.claudeCopy("b.md"); !ok || got != "from the peer\n" {
		t.Errorf("a pulled note must be propagated into the Claude tree: %q %v", got, ok)
	}
	if f.originHas("mine.md") {
		t.Error("a local-only note reached origin")
	}
	if a := f.autoCommitsOnOrigin(); a != "" {
		t.Errorf("origin has commits from this host: %s", a)
	}
	// Nothing is left half-staged in the mirror either.
	if st := mustGit(t, f.repo, "status", "--porcelain"); strings.TrimSpace(st) != "" {
		t.Errorf("mirror work-tree should stay clean: %s", st)
	}
	if _, err := os.Stat(filepath.Join(f.roots.Mirror, "p1", "memory", "mine.md")); err == nil {
		t.Error("a read-only host must not mirror local notes")
	}
	// A peer deletion arrives too.
	f.peerPush("b.md", "")
	f.loop.pullTick(f.repo, f.roots)
	if _, ok := f.claudeCopy("b.md"); ok {
		t.Error("a pulled deletion must be propagated")
	}
}

func TestReadOnly_StartupCycleAndWatcherNeverCommit(t *testing.T) {
	f := newPullFixture(t, map[string]string{"a.md": "a\n"})
	captureLog(t)
	f.loop.Cfg.ReadOnly = true
	originBefore, headBefore := f.originHead(), f.head()
	writeMem(t, f.roots.Claude, "p1", "mine.md", "only here\n")
	f.restart() // Run: startup reconcile + flush
	if f.head() != headBefore || f.originHead() != originBefore {
		t.Error("the startup cycle of a read-only host committed or pushed")
	}
	handled, err := f.loop.handleEvent((*fsnotify.Watcher)(nil), f.roots, fsnotify.Event{Name: filepath.Join(f.roots.Claude, "p1", "memory", "mine.md"), Op: fsnotify.Write})
	if handled || err != nil {
		t.Errorf("a file event must be ignored: %v %v", handled, err)
	}
	if _, err := os.Stat(filepath.Join(f.roots.Mirror, "p1", "memory", "mine.md")); err == nil {
		t.Error("the watcher path mirrored a local note")
	}
}

func TestReadOnly_CommitMirrorRefuses(t *testing.T) {
	repo, cfg := commitFixture(t)
	cfg.ReadOnly = true
	writeRepoFile(t, repo, "projects/p1/memory/x.md", "x\n")
	committed, err := CommitMirror(repo, cfg, "auto: test")
	if !errors.Is(err, ErrReadOnly) || committed {
		t.Fatalf("want ErrReadOnly, got %v %v", committed, err)
	}
	if out, _ := repo.Run("diff", "--cached", "--name-only"); strings.TrimSpace(out) != "" {
		t.Errorf("a refused commit must stage nothing: %q", out)
	}
}

func TestPopulateClaude_CopiesMirrorNotesAndSkipsNothingElse(t *testing.T) {
	f := newPullFixture(t, map[string]string{"a.md": "a\n", "MEMORY.md": "- [a](a.md)\n"})
	if err := os.RemoveAll(f.roots.Claude); err != nil {
		t.Fatal(err)
	}
	n, err := PopulateClaude(f.roots)
	if err != nil || n != 2 {
		t.Fatalf("%d %v", n, err)
	}
	if got, _ := f.claudeCopy("a.md"); got != "a\n" {
		t.Errorf("a.md = %q", got)
	}
	if n, _ := PopulateClaude(f.roots); n != 0 {
		t.Errorf("a second run changes nothing, copied %d", n)
	}
}

// A host that was writable and is now read-only may hold commits it never
// pushed. They stay where they are.
func TestReadOnly_NeverPushesAnEarlierLocalCommit(t *testing.T) {
	f := newPullFixture(t, map[string]string{"a.md": "a\n"})
	captureLog(t)
	writeRepoFile(t, f.repo, "projects/p1/memory/early.md", "committed before the switch\n")
	mustGit(t, f.repo, "add", "-A")
	mustGit(t, f.repo, "commit", "-q", "-m", "auto: earlier")
	originBefore := f.originHead()
	f.loop.Cfg.ReadOnly = true
	f.loop.pullTick(f.repo, f.roots)
	if f.originHead() != originBefore || f.originHas("early.md") {
		t.Error("a read-only host pushed an earlier local commit")
	}
}

// With archiveDeletes on, a pull can bring an archive copy whose bytes no
// longer match its name; a writable host repairs that with a commit. A
// read-only host must leave it alone.
func TestReadOnly_NeverCommitsAnArchiveRepair(t *testing.T) {
	archClock(t)
	captureLog(t)
	f := newPullFixture(t, map[string]string{"a.md": "a\n"})
	f.loop.Cfg.ArchiveDeletes = true
	f.loop.Cfg.ReadOnly = true
	v0, v1 := bigNote+"v0\n", bigNote+"v1 edited\n"
	p := archPath("p1", "x.md", v0)
	f.peerWrite(p, v0) // a proper archive copy
	f.loop.pullTick(f.repo, f.roots)
	f.peerWrite(p, v1) // an edit merged into the copy: the name no longer matches
	f.loop.pullTick(f.repo, f.roots)
	if f.head() != f.originHead() {
		t.Errorf("a read-only host made a commit of its own (repair): head %s, origin %s", f.head(), f.originHead())
	}
	if n := f.autoCommitsOnOrigin(); n != "" {
		t.Errorf("origin has commits from this host: %s", n)
	}
}
