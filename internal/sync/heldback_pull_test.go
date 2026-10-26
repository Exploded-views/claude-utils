package sync

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/merge"
)

// These tests cover what happens to a file the scrub holds back when the
// daemon then pulls. A held-back file stays modified (or untracked) in the
// mirror work-tree, so `git pull --rebase --autostash` runs on a dirty tree,
// and propagateChanges copies whatever the pull left in the mirror into the
// Claude tree. The property under test: while a file is held back, its
// authoritative copy in the Claude tree must not change.

// mergeDriverEnv makes the test binary act as claude-memmerge when git runs it
// as the MEMORY.md merge driver (see TestMain).
const mergeDriverEnv = "MEMSYNC_TEST_AS_MERGE_DRIVER"

func TestMain(m *testing.M) {
	if os.Getenv(mergeDriverEnv) == "1" && len(os.Args) >= 4 {
		os.Exit(runAsMergeDriver(os.Args[1], os.Args[2], os.Args[3]))
	}
	os.Exit(m.Run())
}

// runAsMergeDriver mirrors cmd/claude-memmerge: merge base/ours/theirs and
// write the result over ours. Exit 1 on conflict.
func runAsMergeDriver(basePath, oursPath, theirsPath string) int {
	base, err1 := os.ReadFile(basePath)
	ours, err2 := os.ReadFile(oursPath)
	theirs, err3 := os.ReadFile(theirsPath)
	if err1 != nil || err2 != nil || err3 != nil {
		return 2
	}
	merged, conflict := merge.Merge(string(base), string(ours), string(theirs))
	if err := os.WriteFile(oursPath, []byte(merged), 0600); err != nil {
		return 2
	}
	if conflict {
		return 1
	}
	return 0
}

// pullFixture is one origin (a local bare repo), this PC (sync work-tree plus
// Claude tree, both in temp dirs) and a peer clone that pushes changes.
type pullFixture struct {
	t      *testing.T
	origin string
	repo   *gitwt.Repo
	peer   *gitwt.Repo
	roots  Roots
	loop   *Loop
}

func newPullFixture(t *testing.T, seed map[string]string) *pullFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	t.Setenv(mergeDriverEnv, "1")

	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", "-b", "main", origin).CombinedOutput(); err != nil {
		t.Fatalf("init bare: %v: %s", err, out)
	}

	// The peer seeds origin with the shared state both PCs start from.
	peer := gitwt.New(filepath.Join(root, "peer"))
	if err := peer.Clone(origin); err != nil {
		t.Fatal(err)
	}
	writeRepoFile(t, peer, ".gitattributes", "MEMORY.md merge=claude-memory-index\n")
	writeRepoFile(t, peer, ".gitignore", "config.json\ndaemon.pid\n.state/\ndenylist.txt\n*.tmp\n*.tmp.*\n*.from-remote-*\n")
	for rel, content := range seed {
		writeRepoFile(t, peer, "projects/p1/memory/"+rel, content)
	}
	mustGit(t, peer, "add", "-A")
	mustGit(t, peer, "commit", "-q", "-m", "seed")
	mustGit(t, peer, "push", "-q", "origin", "main")

	syncDir := filepath.Join(root, "sync")
	repo := gitwt.New(syncDir)
	if err := repo.Clone(origin); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "config", "merge.claude-memory-index.name", "claude memory index merge")
	mustGit(t, repo, "config", "merge.claude-memory-index.driver",
		`"`+filepath.ToSlash(self)+`" %O %A %B %L %P`)

	claude := filepath.Join(root, "claude", "projects")
	roots := Roots{Claude: claude, Mirror: filepath.Join(syncDir, "projects")}
	for rel, content := range seed {
		writeMem(t, claude, "p1", rel, content)
	}

	cfg := config.Defaults()
	cfg.SyncDir = syncDir
	cfg.ClaudeProjectsDir = claude
	return &pullFixture{
		t: t, origin: origin, repo: repo, peer: peer, roots: roots,
		loop: &Loop{Cfg: cfg, Branch: "main", Hostname: "pc-a"},
	}
}

func mustGit(t *testing.T, r *gitwt.Repo, args ...string) string {
	t.Helper()
	out, err := r.Run(args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// editLocal is what the watcher does: write the Claude copy, mirror it.
func (f *pullFixture) editLocal(name, content string) {
	f.t.Helper()
	writeMem(f.t, f.roots.Claude, "p1", name, content)
	if err := CopyToMirror(f.roots, "p1", name); err != nil {
		f.t.Fatal(err)
	}
}

func (f *pullFixture) peerPush(name, content string) {
	f.t.Helper()
	mustGit(f.t, f.peer, "pull", "-q", "origin", "main")
	rel := "projects/p1/memory/" + name
	if content == "" {
		mustGit(f.t, f.peer, "rm", "-q", rel)
	} else {
		writeRepoFile(f.t, f.peer, rel, content)
		mustGit(f.t, f.peer, "add", rel)
	}
	mustGit(f.t, f.peer, "commit", "-q", "-m", "peer: "+name)
	mustGit(f.t, f.peer, "push", "-q", "origin", "main")
}

// peerWrite pushes a file at an arbitrary repo path (peerPush is memory-only).
func (f *pullFixture) peerWrite(rel, content string) {
	f.t.Helper()
	mustGit(f.t, f.peer, "pull", "-q", "origin", "main")
	writeRepoFile(f.t, f.peer, rel, content)
	mustGit(f.t, f.peer, "add", "-A")
	mustGit(f.t, f.peer, "commit", "-q", "-m", "peer: "+rel)
	mustGit(f.t, f.peer, "push", "-q", "origin", "main")
}

func (f *pullFixture) originHas(rel string) bool {
	out, _ := f.repo.Run("ls-tree", "-r", "--name-only", "origin/main")
	return strings.Contains(out, rel)
}

func (f *pullFixture) claudeCopy(name string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(f.roots.Claude, "p1", "memory", name))
	if err != nil {
		return "", false
	}
	return string(b), true
}

// redact keeps the fake credential out of test output.
func redact(s string) string { return strings.ReplaceAll(s, fakeSecret(), "<SECRET>") }

// requirePreserved is the property: the Claude copy of a held-back file still
// holds every line the user wrote and no conflict markers. (Lines the peer
// added may be merged in; that is not a loss.)
func (f *pullFixture) requirePreserved(name, want string, flushErr error) {
	f.t.Helper()
	got, ok := f.claudeCopy(name)
	status, _ := f.repo.Run("status", "--porcelain")
	stash, _ := f.repo.Run("stash", "list")
	f.t.Logf("flush err: %v", flushErr)
	f.t.Logf("mirror status:\n%s", status)
	f.t.Logf("stash list: %q", stash)
	if m, err := os.ReadFile(filepath.Join(f.roots.Mirror, "p1", "memory", name)); err == nil {
		f.t.Logf("mirror copy still holds the local version: %v", string(m) == want)
	} else {
		f.t.Logf("mirror copy: %v", err)
	}
	if !ok {
		f.t.Fatalf("Claude copy of held-back %s was deleted", name)
	}
	lost := ""
	for _, line := range strings.SplitAfter(want, "\n") {
		if line != "" && !strings.Contains(got, line) {
			lost += line
		}
	}
	if lost != "" || strings.Contains(got, "<<<<<<<") || strings.Contains(got, ">>>>>>>") {
		f.t.Fatalf("Claude copy of held-back %s was overwritten:\n--- wrote\n%s--- now\n%s--- lines lost\n%s",
			name, redact(want), redact(got), redact(lost))
	}
}

// A held-back, tracked file that the peer also changes on the same line.
func TestHeldBackPull_TrackedFileChangedUpstream(t *testing.T) {
	f := newPullFixture(t, map[string]string{"note.md": "alpha\nbeta\ngamma\n"})
	captureLog(t)
	local := "alpha\nbeta token: " + fakeSecret() + "\ngamma\n"
	f.editLocal("note.md", local)
	f.peerPush("note.md", "alpha\nbeta edited on peer\ngamma\n")

	err := f.loop.flush(f.repo, f.roots, true)
	f.requirePreserved("note.md", local, err)
}

// Once the user fixes a held-back file, it is committed over whatever the peer
// pushed meanwhile. The peer's edit must survive somewhere: in HEAD (merged)
// or as a .from-remote-* backup in the mirror.
func TestHeldBackPull_UpstreamEditSurvivesRelease(t *testing.T) {
	f := newPullFixture(t, map[string]string{"note.md": "alpha\nbeta\ngamma\n"})
	captureLog(t)
	f.editLocal("note.md", "alpha\nbeta token: "+fakeSecret()+"\ngamma\n")
	f.peerPush("note.md", "alpha\nbeta edited on peer\ngamma\n")
	if err := f.loop.flush(f.repo, f.roots, true); err != nil {
		t.Logf("first flush: %v", err)
	}
	// The user removes the secret from what they see in the Claude tree.
	f.editLocal("note.md", "alpha\nbeta (redacted)\ngamma\n")
	if err := f.loop.flush(f.repo, f.roots, true); err != nil {
		t.Logf("second flush: %v", err)
	}

	const peerLine = "beta edited on peer"
	if head, _ := f.repo.Run("show", "HEAD:projects/p1/memory/note.md"); strings.Contains(head, peerLine) {
		return
	}
	backups, _ := filepath.Glob(filepath.Join(f.roots.Mirror, "p1", "memory", "note.md.from-remote-*"))
	for _, b := range backups {
		if c, err := os.ReadFile(b); err == nil && strings.Contains(string(c), peerLine) {
			return
		}
	}
	head, _ := f.repo.Run("show", "HEAD:projects/p1/memory/note.md")
	t.Fatalf("peer's edit lost: not in HEAD (%q) and no .from-remote backup", redact(head))
}

// Same, but this PC also has a clean committed change, so the pull is a real
// rebase rather than a fast-forward.
func TestHeldBackPull_TrackedFileChangedUpstreamWithLocalCommit(t *testing.T) {
	f := newPullFixture(t, map[string]string{"note.md": "alpha\nbeta\ngamma\n"})
	captureLog(t)
	local := "alpha\nbeta token: " + fakeSecret() + "\ngamma\n"
	f.editLocal("note.md", local)
	f.editLocal("clean.md", "clean local note\n")
	f.peerPush("note.md", "alpha\nbeta edited on peer\ngamma\n")

	err := f.loop.flush(f.repo, f.roots, true)
	f.requirePreserved("note.md", local, err)
}

// requireDenylist with no denylist holds back every change, secret or not.
func TestHeldBackPull_RequireDenylistMissingChangedUpstream(t *testing.T) {
	f := newPullFixture(t, map[string]string{"note.md": "alpha\nbeta\ngamma\n"})
	f.loop.Cfg.RequireDenylist = true
	captureLog(t)
	local := "alpha\nbeta edited here\ngamma\n"
	f.editLocal("note.md", local)
	f.peerPush("note.md", "alpha\nbeta edited on peer\ngamma\n")

	err := f.loop.flush(f.repo, f.roots, true)
	f.requirePreserved("note.md", local, err)
}

// A held-back file that was never committed, while the peer adds a different
// file at the same path.
func TestHeldBackPull_UntrackedFileAddedUpstream(t *testing.T) {
	f := newPullFixture(t, map[string]string{"other.md": "x\n"})
	captureLog(t)
	local := "mine token: " + fakeSecret() + "\n"
	f.editLocal("new.md", local)
	f.peerPush("new.md", "theirs\n")

	err := f.loop.flush(f.repo, f.roots, true)
	f.requirePreserved("new.md", local, err)

	// Liveness: a later clean change on this PC must still reach origin.
	f.editLocal("later.md", "clean\n")
	err = f.loop.flush(f.repo, f.roots, true)
	if out, _ := f.repo.Run("ls-tree", "-r", "--name-only", "origin/main"); !strings.Contains(out, "later.md") {
		t.Errorf("clean later.md never reached origin while new.md is held back (flush err: %v)", err)
	}
}

// A held-back, tracked file that the peer deletes.
func TestHeldBackPull_TrackedFileDeletedUpstream(t *testing.T) {
	f := newPullFixture(t, map[string]string{"note.md": "alpha\n", "keep.md": "k\n"})
	captureLog(t)
	local := "alpha token: " + fakeSecret() + "\n"
	f.editLocal("note.md", local)
	f.peerPush("note.md", "")

	err := f.loop.flush(f.repo, f.roots, true)
	f.requirePreserved("note.md", local, err)
}

// The same deletion, through the running daemon's startup cycle.
func TestHeldBackPull_TrackedFileDeletedUpstreamDaemon(t *testing.T) {
	f := newPullFixture(t, map[string]string{"note.md": "alpha\n", "keep.md": "k\n"})
	captureLog(t)
	local := "alpha token: " + fakeSecret() + "\n"
	f.editLocal("note.md", local)
	f.peerPush("note.md", "")

	f.loop.Cfg.DebounceMs = 50
	flushes := make(chan error, 16)
	f.loop.OnFlush = func(_ bool, err error) { flushes <- err }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel) // a failing test must not leave the loop running
	done := make(chan error, 1)
	go func() { done <- f.loop.Run(ctx) }()

	// The startup flush is the cycle under test: it pulls the upstream deletion
	// and must leave the held-back Claude copy as it was, neither deleting nor
	// rewriting it. No later flush is awaited: the daemon raises none unless a
	// watched file changes, and a correct cycle changes nothing in the Claude tree.
	lastErr := awaitFlush(t, flushes, "startup flush")
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	f.requirePreserved("note.md", local, lastErr)
}

// MEMORY.md goes through the custom merge driver. Both sides add a section.
func TestHeldBackPull_MemoryIndexBothAddSections(t *testing.T) {
	base := "# Memory\n\n## Shared\n- [A](a.md) — a\n"
	f := newPullFixture(t, map[string]string{"MEMORY.md": base})
	captureLog(t)
	local := base + "\n## Local\n- token: \"" + strings.Repeat("q", 20) + "\"\n"
	f.editLocal("MEMORY.md", local)
	f.peerPush("MEMORY.md", base+"\n## Peer\n- [P](p.md) — p\n")

	err := f.loop.flush(f.repo, f.roots, true)
	f.requirePreserved("MEMORY.md", local, err)
}

// MEMORY.md where both sides edit the same free-form section body.
func TestHeldBackPull_MemoryIndexSameSection(t *testing.T) {
	base := "# Memory\n\n## Notes\nsome prose line\n"
	f := newPullFixture(t, map[string]string{"MEMORY.md": base})
	captureLog(t)
	local := "# Memory\n\n## Notes\nsome prose line token: " + fakeSecret() + "\n"
	f.editLocal("MEMORY.md", local)
	f.peerPush("MEMORY.md", "# Memory\n\n## Notes\nsome prose line, edited on peer\n")

	err := f.loop.flush(f.repo, f.roots, true)
	f.requirePreserved("MEMORY.md", local, err)
}
