package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// secretName is a file name the test denylist lists. Nothing the daemon logs or
// returns may contain it, whichever error path is taken. It is not in any test
// name, since temp dirs are named after the test.
const secretName = "nightjar-notes.md"

// redactFixture is a pullFixture whose denylist lists secretName's stem.
func redactFixture(t *testing.T, seed map[string]string) *pullFixture {
	t.Helper()
	f := newPullFixture(t, seed)
	if err := os.WriteFile(f.loop.Cfg.DenylistPath(), []byte("nightjar\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return f
}

func requireNoName(t *testing.T, what, text string) {
	t.Helper()
	if strings.Contains(strings.ToLower(text), "nightjar") {
		t.Errorf("%s names a denylisted file:\n%s", what, text)
	}
}

// blockDir makes path a directory, so a rename of a file onto it fails.
func blockDir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
}

// propagateChanges: the copy into the Claude tree fails for a denylisted name.
func TestRedact_PropagateCopyError(t *testing.T) {
	f := redactFixture(t, map[string]string{"keep.md": "k\n"})
	logs := captureLog(t)
	f.peerPush(secretName, "peer\n")
	blockDir(t, filepath.Join(f.roots.Claude, "p1", "memory", secretName))

	err := f.loop.flush(f.repo, f.roots, true)
	if !strings.Contains(logs.String(), "propagate") {
		t.Fatalf("the copy error was not triggered; log:\n%s", logs)
	}
	requireNoName(t, "log", logs.String())
	if err != nil {
		requireNoName(t, "flush error", err.Error())
	}
}

// `git add` fails on a denylisted file that cannot be read: git names it in stderr.
func TestRedact_GitAddError(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs permission bits and a non-root user")
	}
	f := redactFixture(t, map[string]string{"keep.md": "k\n"})
	logs := captureLog(t)
	f.peerPush("peer.md", "p\n") // so the idle gate does not skip the cycle
	f.editLocal(secretName, "mine\n")
	p := filepath.Join(f.roots.Mirror, "p1", "memory", secretName)
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(p, 0600) })

	err := f.loop.flush(f.repo, f.roots, true)
	if err == nil {
		t.Fatalf("the park error was not triggered; log:\n%s", logs)
	}
	requireNoName(t, "flush error", err.Error())
	requireNoName(t, "log", logs.String())
}

// finishParked via flush: restoring a denylisted memory file fails (CopyToMirror
// and writeAtomic), and so does restoring a denylisted file outside projects/.
func TestRedact_UnparkErrors(t *testing.T) {
	f := redactFixture(t, map[string]string{"keep.md": "k\n"})
	logs := captureLog(t)
	f.peerPush("peer.md", "p\n")
	f.editLocal(secretName, "mine\n")
	f.writeWork("distilled/"+secretName, "mine\n")

	old := propagate
	propagate = func(*gitwt.Repo, Roots, string, string, map[string]bool, *pathGuard) error {
		// Mid-cycle, after the park: get in the way of both restores.
		blockDir(t, filepath.Join(f.roots.Mirror, "p1", "memory", secretName))
		if err := os.RemoveAll(filepath.Join(f.repo.Dir, "distilled")); err != nil {
			t.Fatal(err)
		}
		return os.WriteFile(filepath.Join(f.repo.Dir, "distilled"), []byte("x"), 0600)
	}
	t.Cleanup(func() { propagate = old })

	err := f.loop.flush(f.repo, f.roots, true)
	if err == nil || !strings.Contains(err.Error(), "restore held-back files") {
		t.Fatalf("the unpark errors were not triggered: %v", err)
	}
	requireNoName(t, "flush error", err.Error())
	requireNoName(t, "log", logs.String())
}

// A git failure whose stderr names a denylisted file (here from a hook).
func TestRedact_GitStderrFromFailedPull(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell hook")
	}
	f := redactFixture(t, map[string]string{"keep.md": "k\n"})
	logs := captureLog(t)
	hook := "#!/bin/sh\necho 'cannot rebase " + secretName + "' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(f.repo.Dir, ".git", "hooks", "pre-rebase"), []byte(hook), 0700); err != nil {
		t.Fatal(err)
	}
	f.editLocal("mine.md", "m\n") // a local commit forces a real rebase
	f.peerPush("peer.md", "p\n")

	err := f.loop.flush(f.repo, f.roots, true)
	if !strings.Contains(logs.String(), "pull:") {
		t.Fatalf("the pull error was not logged; log:\n%s", logs)
	}
	requireNoName(t, "log", logs.String())
	if err != nil {
		requireNoName(t, "flush error", err.Error())
	}
}

// handleEvent: mirroring a denylisted file fails.
func TestRedact_HandleEventErrors(t *testing.T) {
	f := redactFixture(t, map[string]string{"keep.md": "k\n"})
	captureLog(t)
	w, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	writeMem(t, f.roots.Claude, "p1", secretName, "mine\n")
	blockDir(t, filepath.Join(f.roots.Mirror, "p1", "memory", secretName))

	_, err = f.loop.handleEvent(w, f.roots, fsnotify.Event{
		Name: filepath.Join(f.roots.Claude, "p1", "memory", secretName), Op: fsnotify.Write,
	})
	if err == nil {
		t.Fatal("the copy error was not triggered")
	}
	requireNoName(t, "handleEvent error", err.Error())
}

// Run logs a Reconcile failure; the path is redacted there too.
func TestRedact_RunReconcileError(t *testing.T) {
	f := redactFixture(t, map[string]string{"keep.md": "k\n"})
	logs := captureLog(t)
	writeMem(t, f.roots.Claude, "p1", secretName, "mine\n")
	blockDir(t, filepath.Join(f.roots.Mirror, "p1", "memory", secretName))

	flushed := make(chan error, 4)
	f.loop.OnFlush = func(_ bool, err error) { flushed <- err }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.loop.Run(ctx) }()
	select {
	case <-flushed:
	case <-time.After(10 * time.Second):
		t.Fatal("no startup flush")
	}
	cancel()
	<-done
	if !strings.Contains(logs.String(), "reconcile") {
		t.Fatalf("the reconcile error was not triggered; log:\n%s", logs)
	}
	requireNoName(t, "log", logs.String())
}

// A name that does not match the denylist is still logged, so the redaction
// does not blank every diagnostic.
func TestRedact_OrdinaryNamesStillLogged(t *testing.T) {
	f := redactFixture(t, map[string]string{"keep.md": "k\n"})
	logs := captureLog(t)
	f.peerPush("ordinary.md", "peer\n")
	blockDir(t, filepath.Join(f.roots.Claude, "p1", "memory", "ordinary.md"))

	_ = f.loop.flush(f.repo, f.roots, true)
	if !strings.Contains(logs.String(), "ordinary.md") {
		t.Errorf("an ordinary name should still be logged; log:\n%s", logs)
	}
}

// With a denylist that cannot be parsed nothing can vouch for any name, so no
// path is logged.
func TestRedact_UnreadableDenylistRedactsAllPaths(t *testing.T) {
	f := newPullFixture(t, map[string]string{"keep.md": "k\n"})
	if err := os.WriteFile(f.loop.Cfg.DenylistPath(), []byte("(unclosed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	logs := captureLog(t)
	f.peerPush("ordinary.md", "peer\n")
	blockDir(t, filepath.Join(f.roots.Claude, "p1", "memory", "ordinary.md"))

	_ = f.loop.flush(f.repo, f.roots, true)
	if strings.Contains(logs.String(), "ordinary.md") {
		t.Errorf("a path was logged although the denylist cannot be read:\n%s", logs)
	}
}

func TestPathGuard(t *testing.T) {
	f := newPullFixture(t, nil)
	// An anchored pattern sees repo-relative paths; the other is unanchored.
	if err := os.WriteFile(f.loop.Cfg.DenylistPath(), []byte("^projects/[^/]+/memory/night owl\nheron\n"), 0600); err != nil {
		t.Fatal(err)
	}
	g := newPathGuard(f.loop.Cfg)
	mem := filepath.Join(f.roots.Mirror, "p1", "memory")

	for _, c := range []struct {
		name string
		err  error
		hide bool
	}{
		{"anchored, space in name", &os.PathError{Op: "open", Path: filepath.Join(mem, "night owl.md"), Err: os.ErrPermission}, true},
		{"anchored, other name", &os.PathError{Op: "open", Path: filepath.Join(mem, "day owl.md"), Err: os.ErrPermission}, false},
		{"unanchored, in git output", errors.New("git pull: exit status 1: error: heron.md would be overwritten"), true},
		{"wrapped", fmt.Errorf("restore: %w", &os.PathError{Op: "open", Path: filepath.Join(mem, "heron.md"), Err: os.ErrPermission}), true},
		{"joined", errors.Join(errors.New("fine"), errors.New("bad heron.md")), true},
		{"no path", errors.New("push: exit status 1"), false},
	} {
		got := g.Err(c.err)
		if hidden := got.Error() != c.err.Error(); hidden != c.hide {
			t.Errorf("%s: hidden=%v, want %v: %q", c.name, hidden, c.hide, got)
		}
		if c.hide && (strings.Contains(got.Error(), "heron") || strings.Contains(got.Error(), "night owl")) {
			t.Errorf("%s: leaked: %q", c.name, got)
		}
	}
	wrapped := g.Err(fmt.Errorf("restore held-back files: %w", &os.PathError{Op: "open", Path: filepath.Join(mem, "heron.md"), Err: os.ErrPermission}))
	if !strings.HasPrefix(wrapped.Error(), "restore held-back files: ") || !errors.Is(wrapped, os.ErrPermission) {
		t.Errorf("redaction should keep the context and the error chain: %q", wrapped)
	}
	if g.Path("projects/p1/memory/heron.md") != redactedPath || g.Path("projects/p1/memory/ok.md") != "projects/p1/memory/ok.md" {
		t.Error("Path")
	}
	var none *pathGuard // nil hides every path
	if none.Path("a/b") != redactedPath || none.Err(errors.New("plain")).Error() != "plain" {
		t.Error("nil guard")
	}
}
