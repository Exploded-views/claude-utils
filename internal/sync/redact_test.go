package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/scrub"
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

// guardFor is a pathGuard over a denylist with the given lines.
func guardFor(t *testing.T, patterns ...string) (*pathGuard, *pullFixture) {
	t.Helper()
	f := newPullFixture(t, nil)
	if err := os.WriteFile(f.loop.Cfg.DenylistPath(), []byte(strings.Join(patterns, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return newPathGuard(f.loop.Cfg), f
}

// Only the matching tokens are replaced. The exit status and git's reason stay,
// so the error is still diagnosable, and the chain still unwraps.
func TestRedact_ReplacesOnlyMatchingTokens(t *testing.T) {
	g, f := guardFor(t, "heron")
	_, runErr := f.repo.Run("show", "HEAD:projects/p1/memory/heron.md")
	if runErr == nil {
		t.Fatal("expected git show to fail")
	}
	got := g.Err(fmt.Errorf("pull: %w", runErr)).Error()
	for _, want := range []string{"pull: git show", "exit status 128", "fatal:", redactedPath} {
		if !strings.Contains(got, want) {
			t.Errorf("redacted error lost %q: %q", want, got)
		}
	}
	if strings.Contains(got, "heron") {
		t.Errorf("leaked: %q", got)
	}
	if !errors.Is(g.Err(fmt.Errorf("w: %w", os.ErrPermission)), os.ErrPermission) {
		t.Error("chain lost")
	}
}

// A denylisted word that also sits in the remote URL (an org or firm name)
// must not turn every push or pull error into a placeholder.
func TestRedact_WordInRemoteURLKeepsTheCause(t *testing.T) {
	g, _ := guardFor(t, "heron")
	push := errors.New("git push --set-upstream origin main: exit status 128: fatal: unable to access " +
		"'https://git.example.com/heron/memory.git/': Could not resolve host: git.example.com")
	got := g.Err(fmt.Errorf("push: %w", push)).Error()
	for _, want := range []string{"push: git push", "exit status 128", "unable to access", "Could not resolve host"} {
		if !strings.Contains(got, want) {
			t.Errorf("cause lost, missing %q: %q", want, got)
		}
	}
	if strings.Contains(got, "heron") {
		t.Errorf("leaked: %q", got)
	}
	if strings.Contains(got, "matches the denylist") {
		t.Errorf("claims a path matched: %q", got)
	}
}

// Anchored patterns see the repo-relative path whatever surrounds it: git's
// :(literal) pathspec prefix, quotes, colons, tabs, or an absolute root.
func TestRedact_AnchoredPatternsThroughGitSyntax(t *testing.T) {
	g, f := guardFor(t, "^projects/[^/]+/memory/heron", "^projects/[^/]+/memory/night owl")
	abs := filepath.ToSlash(filepath.Join(f.roots.Mirror, "p1", "memory", "heron.md"))
	for name, text := range map[string]string{
		"literal pathspec":  "error: pathspec ':(literal)projects/p1/memory/heron.md' did not match any file(s) known to git",
		"icase pathspec":    "error: pathspec ':(literal,icase)projects/p1/memory/heron.md' did not match",
		"double quotes":     `error: pathspec "projects/p1/memory/heron.md" did not match`,
		"colon after":       "error: projects/p1/memory/heron.md: needs merge",
		"absolute root":     "open " + abs + ": permission denied",
		"tab-indented":      "overwritten:\n\tprojects/p1/memory/heron.md\nAborting",
		"quoted with space": "error: pathspec 'projects/p1/memory/night owl.md' did not match",
		"bare with space":   "overwritten:\n\tprojects/p1/memory/night owl.md\nAborting",
	} {
		got := g.Err(errors.New(text)).Error()
		if strings.Contains(got, "heron") || strings.Contains(got, "night") || strings.Contains(got, "owl") {
			t.Errorf("%s: leaked: %q", name, got)
		}
		if !strings.Contains(got, redactedPath) {
			t.Errorf("%s: want the placeholder: %q", name, got)
		}
	}
	for _, text := range []string{
		"error: pathspec ':(literal)projects/p1/memory/day.md' did not match",
		"error: projects/p1/memory/day owl.md: needs merge",
	} {
		if got := g.Err(errors.New(text)).Error(); got != text {
			t.Errorf("an unlisted name changed: %q", got)
		}
	}
}

// A long file list is checked in full: there is no cap on the words examined.
func TestRedact_LongFileList(t *testing.T) {
	g, _ := guardFor(t, "^projects/[^/]+/memory/heron")
	for _, n := range []int{30, 500} {
		var list []string
		for i := 0; i < n; i++ {
			list = append(list, fmt.Sprintf("projects/p1/memory/n%03d.md", i))
		}
		list = append(list, "projects/p1/memory/heron.md")
		text := "git pull --rebase --autostash origin main: exit status 1: error: The following untracked " +
			"working tree files would be overwritten by merge:\n\t" + strings.Join(list, "\n\t") +
			"\nPlease move or remove them before you merge.\nAborting"
		got := g.Err(errors.New(text)).Error()
		if strings.Contains(got, "heron") {
			t.Errorf("%d names: leaked", n)
		}
		for _, want := range []string{"exit status 1", "n000.md", "Please move or remove them", "Aborting"} {
			if !strings.Contains(got, want) {
				t.Errorf("%d names: lost %q", n, want)
			}
		}
	}
}

// Each line is also tested as a whole, as it is written: a name that only the
// whole line matches (here, because of the parentheses that word and run
// matching cut off) is caught.
func TestRedact_WholeLine(t *testing.T) {
	g, _ := guardFor(t, `^\(private note\.md\)$`)
	got := g.Err(errors.New("overwritten:\n\t(private note.md)\nAborting")).Error()
	if strings.Contains(got, "private") || !strings.Contains(got, "Aborting") {
		t.Errorf("got %q", got)
	}
}

// A denylisted name of any word count is caught, not only one within a fixed
// window of words, also in the middle of a line that does not start with it.
func TestRedact_NameOfAnyWordCount(t *testing.T) {
	for _, words := range []int{13, 40, 200} {
		name := strings.TrimSpace(strings.Repeat("a b c d e f g h i j ", words/10+1))
		name = strings.Join(strings.Fields(name)[:words], " ") + " heron.md"
		g, _ := guardFor(t, `^projects/[^/]+/memory/`+regexp.QuoteMeta(name)+`$`)
		text := "error: projects/p1/memory/" + name + ": needs merge"
		got := g.Err(errors.New(text)).Error()
		if strings.Contains(got, "heron") || !strings.Contains(got, "needs merge") {
			t.Errorf("%d words: %q", words, got)
		}
	}
}

// A long line with a long name in it, whether the pattern is anchored or not,
// which no fixed window of words may miss.
func TestRedact_LongLineWithLongName(t *testing.T) {
	name := "one two three four five six seven eight nine ten eleven twelve thirteen heron.md"
	for pat, pattern := range map[string]string{
		"anchored":   `^projects/[^/]+/memory/` + regexp.QuoteMeta(name) + `$`,
		"unanchored": regexp.QuoteMeta(name),
	} {
		g, _ := guardFor(t, pattern)
		for _, filler := range []int{600, 20000} {
			line := strings.Repeat("x ", filler) + "projects/p1/memory/" + name + ": needs merge"
			got := g.Err(errors.New(line)).Error()
			if strings.Contains(got, "heron") || strings.Contains(got, "thirteen") {
				t.Errorf("%s, %d fillers: leaked: ...%s", pat, filler, got[len(got)-80:])
			}
			if !strings.HasSuffix(got, "needs merge") {
				t.Errorf("%s, %d fillers: lost the reason: ...%s", pat, filler, got[len(got)-80:])
			}
		}
	}
}

// The cost of a line is linear in its length: one worst-case line at the
// budget, with 10 patterns, anchored and not, finishes well inside the limit.
func TestRedact_WorstCaseLineIsFast(t *testing.T) {
	var pats []string
	for i := 0; i < 5; i++ {
		pats = append(pats, fmt.Sprintf(`nomatch%d\.md$`, i), fmt.Sprintf(`^projects/[^/]+/memory/nomatch%d`, i))
	}
	g, _ := guardFor(t, pats...)
	line := strings.TrimSpace(strings.Repeat("a ", maxLineBytes/2))
	for name, text := range map[string]string{
		"words":  line,
		"quotes": strings.TrimSpace(strings.Repeat("'a b' ", maxLineBytes/6)),
		"colons": strings.TrimSpace(strings.Repeat("a:(b) ", maxLineBytes/6)),
	} {
		start := time.Now()
		got := g.Err(errors.New(text)).Error()
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("%s: %d-char line took %v", name, len(text), d)
		}
		if got != text {
			t.Errorf("%s: nothing matches, but the line changed", name)
		}
	}
}

// A line over the budget is not searched: it is redacted whole, and the lines
// around it are untouched.
func TestRedact_LineOverBudgetIsRedactedWhole(t *testing.T) {
	g, _ := guardFor(t, "heron")
	long := strings.Repeat("a ", maxLineBytes/2+1) + "plain words"
	got := g.Err(errors.New("first\n" + long + "\nheron.md\nlast")).Error()
	if got != "first\n"+redactedPath+"\n"+redactedPath+"\nlast" {
		t.Errorf("got %q", got)
	}
}

// An anchor in a pattern means the start or end of the name, which in an error
// line is a delimiter: the word inside a longer name is not the name.
func TestRedact_AnchorsMeanNameBoundaries(t *testing.T) {
	g, _ := guardFor(t, "^heron$")
	for _, text := range []string{
		"unable to access 'https://git.example.com/heron/memory.git/': Could not resolve host",
		"error: projects/p1/memory/heron.md: needs merge",
	} {
		if got := g.Err(errors.New(text)).Error(); got != text {
			t.Errorf("changed: %q", got)
		}
	}
	got := g.Err(errors.New("error: heron: needs merge")).Error()
	if strings.Contains(got, "heron") || !strings.Contains(got, "needs merge") {
		t.Errorf("got %q", got)
	}
}

// With a denylist that cannot be parsed, paths and file names go but the rest
// of the message stays.
func TestRedact_UnreadableDenylistKeepsTheCause(t *testing.T) {
	f := newPullFixture(t, nil)
	if err := os.WriteFile(f.loop.Cfg.DenylistPath(), []byte("(unclosed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	g := newPathGuard(f.loop.Cfg)
	got := g.Err(errors.New("rename /home/u/.claudesync/projects/p1/memory/a.md: permission denied; also b.md")).Error()
	if strings.Contains(got, "/home") || strings.Contains(got, "a.md") || strings.Contains(got, "b.md") {
		t.Errorf("leaked: %q", got)
	}
	if !strings.Contains(got, "permission denied") {
		t.Errorf("cause lost: %q", got)
	}
}

// The log sites outside flush are guarded too: a failed manifest build names
// the Claude projects dir, whose name here holds a denylisted word.
func TestRedact_RefreshManifestError(t *testing.T) {
	f := redactFixture(t, nil)
	logs := captureLog(t)
	f.loop.Cfg.ClaudeProjectsDir = filepath.Join(t.TempDir(), "nightjar-projects")
	if err := os.WriteFile(f.loop.Cfg.ClaudeProjectsDir, []byte("not a dir"), 0600); err != nil {
		t.Fatal(err)
	}
	f.loop.refreshManifest(Roots{Claude: f.loop.Cfg.ClaudeProjectsDir})
	if !strings.Contains(logs.String(), "build manifest") {
		t.Fatalf("the error was not triggered; log:\n%s", logs)
	}
	requireNoName(t, "log", logs.String())
}

// handleEvent's vanished-file race removes the mirror copy; that error is
// guarded like the others.
func TestRedact_HandleEventVanishedFileRemoveError(t *testing.T) {
	f := redactFixture(t, map[string]string{"keep.md": "k\n"})
	captureLog(t)
	mirrorCopy := filepath.Join(f.roots.Mirror, "p1", "memory", secretName)
	blockDir(t, filepath.Join(mirrorCopy, "sub")) // a non-empty dir: Remove fails
	_, err := f.loop.handleEvent(nil, f.roots, fsnotify.Event{
		Name: filepath.Join(f.roots.Claude, "p1", "memory", secretName), Op: fsnotify.Write,
	})
	if err == nil {
		t.Fatal("the remove error was not triggered")
	}
	requireNoName(t, "handleEvent error", err.Error())
}

// winGuard is a guard whose roots are the slashed forms of Windows paths, which
// is what newPathGuard builds there, with the case folding Windows has.
func winGuard(t *testing.T, patterns ...string) *pathGuard {
	t.Helper()
	deny, err := scrub.ParseDenylist(strings.NewReader(strings.Join(patterns, "\n") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return &pathGuard{scanner: scrub.New(deny), foldCase: foldsCase("windows"), roots: []rootPrefix{
		{`C:/Users/u/.claudesync`, ""},
		{`C:/Users/u/.claude/projects`, "projects"},
	}}
}

// Go and git print Windows paths with backslashes, but the roots are slashed. An
// anchored pattern must still see the path as the repo-relative one it is
// written against.
func TestRedact_WindowsAbsolutePaths(t *testing.T) {
	for _, pat := range []string{`^projects/[^/]+/memory/heron\.md$`, `heron`, `^projects/p1/memory/heron\.md$`} {
		g := winGuard(t, pat)
		for _, text := range []string{
			`open C:\Users\u\.claudesync\projects\p1\memory\heron.md: Access is denied.`,
			`write claude C:\Users\u\.claude\projects\p1\memory\heron.md: The process cannot access the file`,
			`open c:\users\U\.CLAUDESYNC\projects\p1\memory\heron.md: Access is denied.`, // drive letter and directories fold
			`open "C:/Users/u/.claudesync/projects/p1/memory/heron.md": denied`,
		} {
			got := g.Err(errors.New(text)).Error()
			if strings.Contains(got, "heron") {
				t.Errorf("%s: leaked: %q", pat, got)
			}
			if !strings.Contains(got, redactedPath) {
				t.Errorf("%s: no placeholder: %q", pat, got)
			}
		}
	}
}

// On a case-sensitive system a root that differs in case is another directory.
func TestRedact_RootCaseFoldsOnlyOnWindowsAndMac(t *testing.T) {
	for goos, folds := range map[string]bool{"windows": true, "darwin": true, "linux": false} {
		if foldsCase(goos) != folds {
			t.Errorf("foldsCase(%q) = %v", goos, !folds)
		}
	}
	g := winGuard(t, `^projects/[^/]+/memory/heron\.md$`)
	g.foldCase = false
	text := `open C:\USERS\u\.claudesync\projects\p1\memory\heron.md: denied`
	if got := g.Err(errors.New(text)).Error(); !strings.Contains(got, "heron") {
		t.Errorf("the other directory was rewritten: %q", got)
	}
}

// A line the root rewrite maps back to the original must replace exactly the
// path, whatever its slashes were.
func TestRedact_WindowsKeepsSurroundingText(t *testing.T) {
	g := winGuard(t, `^projects/[^/]+/memory/heron\.md$`)
	got := g.Err(errors.New(`open C:\Users\u\.claudesync\projects\p1\memory\heron.md: Access is denied.`)).Error()
	if got != "open "+redactedPath+": Access is denied." {
		t.Errorf("got %q", got)
	}
}

// A sentence-ending period or a trailing slash follows a name with a space.
func TestRedact_AnchoredNameWithSpaceBeforePeriod(t *testing.T) {
	g, _ := guardFor(t, `^projects/[^/]+/memory/night owl\.md$`)
	for _, text := range []string{
		"could not restore projects/p1/memory/night owl.md.",
		"could not restore projects/p1/memory/night owl.md/",
	} {
		if got := g.Err(errors.New(text)).Error(); strings.Contains(got, "owl") {
			t.Errorf("leaked: %q", got)
		}
	}
}
