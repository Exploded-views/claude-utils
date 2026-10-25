package route

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// idEnv is a router whose remotes, root commits and clock the test controls.
type idEnv struct {
	r       *Router
	remotes map[string]string
	roots   map[string][]string // missing or nil: roots unavailable (shallow)
	now     time.Time
	log     *bytes.Buffer
	state   string
	have    func(string) bool // nil: every repo is present
}

func newIDEnv(t *testing.T, f File) *idEnv {
	t.Helper()
	e := &idEnv{remotes: map[string]string{}, roots: map[string][]string{}, now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		log: &bytes.Buffer{}, state: filepath.Join(t.TempDir(), "owners.json")}
	e.r = e.router(f)
	return e
}

// router builds a fresh Router (a daemon restart) on the same state files.
func (e *idEnv) router(f File) *Router {
	r := New(f, func(n string) bool { return e.have == nil || e.have(n) })
	r.StatePath = e.state
	r.Writable()
	r.Resolve = func(folder string) (string, resolution) {
		if k, ok := e.remotes[folder]; ok {
			return k, resolutionOK
		}
		return "", resolutionNone
	}
	r.Roots = func(folder string) ([]string, bool) {
		rs := e.roots[folder]
		return rs, len(rs) > 0
	}
	r.Now = func() time.Time { return e.now }
	r.Logf = func(format string, a ...any) { fmt.Fprintf(e.log, format+"\n", a...) }
	return r
}

func (e *idEnv) day(n int) { e.now = e.now.Add(time.Duration(n) * 24 * time.Hour) }

func idFile(routes ...Rule) File { return fileOf(routes...) }

func TestAliasMatchesLikeMainPattern(t *testing.T) {
	f := fileOf(Rule{Remote: "github.com/New-Org/x", Aliases: []string{"GitHub.com/Old-Owner/x", "github.com/other/*"}, Repo: "projects"})
	r := testRouter(t, f, []string{"personal", "projects"}, map[string]string{
		"-a": "github.com/new-org/x", "-b": "github.com/old-owner/x", "-c": "github.com/other/y", "-d": "github.com/zzz/x",
	})
	for folder, want := range map[string]string{"-a": "projects", "-b": "projects", "-c": "projects", "-d": ""} {
		if d := r.Decide(folder); d.Repo != want {
			t.Fatalf("%s: got %q want %q", folder, d.Repo, want)
		}
	}
}

func TestAliasValidation(t *testing.T) {
	f := fileOf(Rule{Folder: "-x", Aliases: []string{"github.com/a/b"}, Repo: "projects"})
	if err := f.validate(); err == nil {
		t.Fatal("aliases on a folder rule accepted")
	}
}

func TestRenameToKeyOfSameRepoUpdatesSilently(t *testing.T) {
	e := newIDEnv(t, idFile(Rule{Remote: "github.com/old/x", Aliases: []string{"github.com/new/x"}, Repo: "projects"}))
	e.remotes["-f"], e.roots["-f"] = "github.com/old/x", []string{"aaa"}
	if d := e.r.Decide("-f"); d.Repo != "projects" || d.Note != "" {
		t.Fatal(d)
	}
	e.log.Reset()
	e.remotes["-f"] = "github.com/new/x"
	e.r.Refresh()
	if d := e.r.Decide("-f"); d.Repo != "projects" || d.Note != "" {
		t.Fatalf("got %+v", d)
	}
	if e.log.Len() != 0 {
		t.Fatalf("logged %q", e.log.String())
	}
	b, _ := os.ReadFile(filepath.Join(filepath.Dir(e.state), "identity.json"))
	if !strings.Contains(string(b), "github.com/new/x") {
		t.Fatalf("record not updated: %s", b)
	}
}

func TestRenameUnmatchedKeepsRoutingThenDeniesAfterGrace(t *testing.T) {
	f := idFile(Rule{Remote: "github.com/old/x", Repo: "projects"})
	e := newIDEnv(t, f)
	e.remotes["-secret-f"], e.roots["-secret-f"] = "github.com/old/x", []string{"aaa"}
	e.r.Decide("-secret-f")
	e.remotes["-secret-f"] = "github.com/new/x"
	e.r.Refresh()
	d := e.r.Decide("-secret-f")
	if d.Repo != "projects" || d.Note != NoteRenamed {
		t.Fatalf("got %+v", d)
	}
	for i := 0; i < 3; i++ {
		e.r.Refresh()
		e.r.Decide("-secret-f")
	}
	out := e.log.String()
	if n := strings.Count(out, "remote name changed"); n != 1 || strings.Contains(out, "secret") {
		t.Fatalf("log: %q", out)
	}
	if s := e.r.Summarize([]string{"-secret-f"}); len(s.Renamed) != 1 || s.Renamed[0].DaysLeft != 30 {
		t.Fatalf("summary %+v", s)
	}
	e.day(29)
	e.r.Refresh()
	if d := e.r.Decide("-secret-f"); d.Repo != "projects" {
		t.Fatalf("day 29: %+v", d)
	}
	e.day(2)
	// A restart keeps the grace clock.
	r2 := e.router(f)
	d = r2.Decide("-secret-f")
	if d.Repo != "" || d.Reason != ReasonRenameExpired {
		t.Fatalf("day 31: %+v", d)
	}
	if s := r2.Summarize([]string{"-secret-f"}); len(s.Expired) != 1 {
		t.Fatalf("summary %+v", s)
	}
}

func TestRenameGraceDaysConfigurable(t *testing.T) {
	days := 2
	f := idFile(Rule{Remote: "github.com/old/x", Repo: "projects"})
	f.RenameGraceDays = &days
	e := newIDEnv(t, f)
	e.remotes["-f"], e.roots["-f"] = "github.com/old/x", []string{"aaa"}
	e.r.Decide("-f")
	e.remotes["-f"] = "github.com/new/x"
	e.r.Refresh()
	if d := e.r.Decide("-f"); d.Repo != "projects" {
		t.Fatal(d)
	}
	e.day(3)
	e.r.Refresh()
	if d := e.r.Decide("-f"); d.Repo != "" {
		t.Fatalf("got %+v", d)
	}
}

func TestAddingAliasEndsRenameState(t *testing.T) {
	f := idFile(Rule{Remote: "github.com/old/x", Repo: "projects"})
	e := newIDEnv(t, f)
	e.remotes["-f"], e.roots["-f"] = "github.com/old/x", []string{"aaa"}
	e.r.Decide("-f")
	e.remotes["-f"] = "github.com/new/x"
	e.r.Refresh()
	e.r.Decide("-f")
	e.r.Replace(idFile(Rule{Remote: "github.com/old/x", Aliases: []string{"github.com/new/x"}, Repo: "projects"}))
	if d := e.r.Decide("-f"); d.Repo != "projects" || d.Note != "" {
		t.Fatalf("got %+v", d)
	}
	e.day(60)
	e.r.Refresh()
	if d := e.r.Decide("-f"); d.Repo != "projects" {
		t.Fatalf("grace clock survived the alias: %+v", d)
	}
	// The record now holds the new name; dropping the alias is the user's
	// choice and denies as it would without any rename handling.
	e.r.Replace(f)
	if d := e.r.Decide("-f"); d.Repo != "" {
		t.Fatalf("after dropping the alias: %+v", d)
	}
}

func TestRenameToKeyRoutedElsewhereDeniesUntilRouteEdited(t *testing.T) {
	f := idFile(
		Rule{Remote: "github.com/old/x", Repo: "projects"},
		Rule{Remote: "github.com/new/**", Repo: "client-a"},
	)
	e := newIDEnv(t, f)
	e.remotes["-f"], e.roots["-f"] = "github.com/old/x", []string{"aaa"}
	e.r.Decide("-f")
	e.remotes["-f"] = "github.com/new/x"
	e.r.Refresh()
	d := e.r.Decide("-f")
	if d.Repo != "" || d.Reason != ReasonRemoteChanged {
		t.Fatalf("got %+v", d)
	}
	if s := e.r.Summarize([]string{"-f"}); len(s.Changed) != 1 {
		t.Fatalf("summary %+v", s)
	}
	// Still denied after a restart and much later.
	e.day(90)
	if d := e.router(f).Decide("-f"); d.Repo != "" || d.Reason != ReasonRemoteChanged {
		t.Fatalf("after restart: %+v", d)
	}
	// Editing the route is the consent.
	e.r.Replace(idFile(
		Rule{Remote: "github.com/old/x", Aliases: []string{"github.com/new/x"}, Repo: "projects"},
		Rule{Remote: "github.com/new/**", Repo: "client-a"},
	))
	if d := e.r.Decide("-f"); d.Repo != "projects" {
		t.Fatalf("after edit: %+v", d)
	}
}

func TestFallbackNeverMovesIntoOrOutOfClientRepo(t *testing.T) {
	// A non-client folder whose new name hits a client rule is denied, not moved in.
	f := idFile(
		Rule{Remote: "github.com/old/x", Repo: "projects"},
		Rule{Remote: "github.com/client/**", Repo: "client-a"},
	)
	e := newIDEnv(t, f)
	e.remotes["-p"], e.roots["-p"] = "github.com/old/x", []string{"aaa"}
	e.r.Decide("-p")
	e.remotes["-p"] = "github.com/client/x"
	e.r.Refresh()
	if d := e.r.Decide("-p"); d.Repo != "" {
		t.Fatalf("moved into client repo: %+v", d)
	}
	// A client folder whose new name hits a broad non-client rule is denied, not moved out.
	f2 := idFile(
		Rule{Remote: "github.com/client/x", Repo: "client-a"},
		Rule{Remote: "github.com/**", Repo: "projects"},
	)
	e2 := newIDEnv(t, f2)
	e2.remotes["-c"], e2.roots["-c"] = "github.com/client/x", []string{"bbb"}
	e2.r.Decide("-c")
	e2.remotes["-c"] = "github.com/moved/x"
	e2.r.Refresh()
	if d := e2.r.Decide("-c"); d.Repo != "" {
		t.Fatalf("moved out of client repo: %+v", d)
	}
	// An unmatched rename of a client folder stays in the client repo only...
	e3 := newIDEnv(t, idFile(Rule{Remote: "github.com/client/x", Repo: "client-a"}))
	e3.remotes["-c"], e3.roots["-c"] = "github.com/client/x", []string{"bbb"}
	e3.r.Decide("-c")
	e3.remotes["-c"] = "github.com/client/x2"
	e3.r.Refresh()
	if d := e3.r.Decide("-c"); d.Repo != "client-a" {
		t.Fatalf("got %+v", d)
	}
	// ...and not once the repo's kind in routes.json no longer says client.
	g := idFile(Rule{Remote: "github.com/client/x", Repo: "client-a"})
	g.Repos["client-a"] = Repo{Config: "/x", Kind: KindProject}
	if d := e3.router(g).Decide("-c"); d.Repo != "" {
		t.Fatalf("fell back out of client kind: %+v", d)
	}
}

func TestMoveFromClientStillLoggedAfterHeldDenial(t *testing.T) {
	f := idFile(
		Rule{Remote: "github.com/client/x", Repo: "client-a"},
		Rule{Remote: "github.com/new/**", Repo: "projects"},
	)
	e := newIDEnv(t, f)
	e.remotes["-c"], e.roots["-c"] = "github.com/client/x", []string{"bbb"}
	e.r.Decide("-c")
	e.remotes["-c"] = "github.com/new/x"
	e.r.Refresh()
	if d := e.r.Decide("-c"); d.Reason != ReasonRemoteChanged {
		t.Fatal(d)
	}
	e.log.Reset()
	// The user edits the rule that catches the new name: consent to the move.
	e.r.Replace(idFile(
		Rule{Remote: "github.com/client/x", Repo: "client-a"},
		Rule{Remote: "github.com/new/x", Repo: "projects"},
	))
	if d := e.r.Decide("-c"); d.Repo != "projects" {
		t.Fatal(d)
	}
	if !strings.Contains(e.log.String(), "moved from client repo") {
		t.Fatalf("log: %q", e.log.String())
	}
}

func TestShallowCloneHasNoFallback(t *testing.T) {
	f := idFile(Rule{Remote: "github.com/old/x", Repo: "projects"})
	e := newIDEnv(t, f)
	e.remotes["-f"] = "github.com/old/x" // no roots: shallow
	if d := e.r.Decide("-f"); d.Repo != "projects" {
		t.Fatal(d)
	}
	e.remotes["-f"] = "github.com/new/x"
	e.roots["-f"] = []string{"aaa"} // even if roots show up later, none was recorded
	e.r.Refresh()
	if d := e.r.Decide("-f"); d.Repo != "" {
		t.Fatalf("got %+v", d)
	}
	// Still shallow at the rename: nothing to compare, so nothing to fall back on.
	delete(e.roots, "-f")
	e.r.Refresh()
	if d := e.r.Decide("-f"); d.Repo != "" {
		t.Fatalf("both unknown: %+v", d)
	}
}

func TestNewFolderSharingRootCommitGetsNoFallback(t *testing.T) {
	f := idFile(Rule{Remote: "github.com/old/x", Repo: "projects"})
	e := newIDEnv(t, f)
	e.remotes["-a"], e.roots["-a"] = "github.com/old/x", []string{"aaa"}
	e.r.Decide("-a")
	// An unrelated fork of the same history, never seen before, under another name.
	e.remotes["-fork"], e.roots["-fork"] = "github.com/someone/x", []string{"aaa"}
	if d := e.r.Decide("-fork"); d.Repo != "" {
		t.Fatalf("new folder got a fallback: %+v", d)
	}
}

func TestDifferentRootsDenyAfterRename(t *testing.T) {
	f := idFile(Rule{Remote: "github.com/old/x", Repo: "projects"})
	e := newIDEnv(t, f)
	e.remotes["-a"], e.roots["-a"] = "github.com/old/x", []string{"aaa", "bbb"}
	e.r.Decide("-a")
	e.remotes["-a"] = "github.com/new/x"
	e.roots["-a"] = []string{"aaa"} // a different set of roots
	e.r.Refresh()
	if d := e.r.Decide("-a"); d.Repo != "" {
		t.Fatalf("got %+v", d)
	}
	e.roots["-a"] = []string{"aaa", "bbb"}
	e.r.Refresh()
	e.log.Reset()
	if d := e.r.Decide("-a"); d.Repo != "projects" || d.Note != NoteRenamed {
		t.Fatalf("multi-root equal set: %+v", d)
	}
}

func TestRootCommitsRealGit(t *testing.T) {
	gitEnv(t)
	root := t.TempDir()
	a := filepath.Join(root, "a")
	os.MkdirAll(a, 0700)
	gitIn(t, a, "init", "-q", "-b", "main")
	commit := func(dir, msg string) {
		os.WriteFile(filepath.Join(dir, msg), []byte(msg), 0600)
		gitIn(t, dir, "add", "-A")
		gitIn(t, dir, "commit", "-q", "-m", msg)
	}
	commit(a, "one")
	commit(a, "two")
	r1, ok := rootCommits(a)
	if !ok || len(r1) != 1 {
		t.Fatalf("roots %v %v", r1, ok)
	}
	// Merged unrelated history: two roots, sorted.
	gitIn(t, a, "checkout", "-q", "--orphan", "other")
	gitIn(t, a, "rm", "-rfq", ".")
	commit(a, "three")
	gitIn(t, a, "checkout", "-q", "main")
	gitIn(t, a, "merge", "-q", "--allow-unrelated-histories", "-m", "m", "other")
	r2, ok := rootCommits(a)
	if !ok || len(r2) != 2 || r2[0] > r2[1] {
		t.Fatalf("roots %v %v", r2, ok)
	}
	// Shallow clone: no roots.
	sh := filepath.Join(root, "shallow")
	gitIn(t, root, "clone", "-q", "--depth", "1", "file://"+a, sh)
	if rs, ok := rootCommits(sh); ok || rs != nil {
		t.Fatalf("shallow clone gave roots %v", rs)
	}
}

// ---- review round: consent, storage, survivors, owner, cache, clock, path ----

func (e *idEnv) idFile() string { return filepath.Join(filepath.Dir(e.state), "identity.json") }

// heldSetup records -f under old/x -> projects, then renames it onto a rule of
// client-a that already existed, so the folder is held.
func heldSetup(t *testing.T) (*idEnv, File) {
	t.Helper()
	f := idFile(
		Rule{Remote: "github.com/old/x", Repo: "projects"},
		Rule{Remote: "github.com/new/**", Repo: "client-a"},
	)
	e := newIDEnv(t, f)
	e.remotes["-f"], e.roots["-f"] = "github.com/old/x", []string{"aaa"}
	e.r.Decide("-f")
	e.remotes["-f"] = "github.com/new/x"
	e.r.Refresh()
	if d := e.r.Decide("-f"); d.Reason != ReasonRemoteChanged {
		t.Fatalf("not held: %+v", d)
	}
	return e, f
}

func TestConsentIgnoresUnrelatedEditsAndReordering(t *testing.T) {
	e, _ := heldSetup(t)
	e.r.Replace(idFile(
		Rule{Remote: "github.com/new/**", Aliases: []string{"github.com/unrelated/y"}, Repo: "client-a"},
		Rule{Remote: "github.com/old/x", Repo: "projects"},
	))
	if d := e.r.Decide("-f"); d.Reason != ReasonRemoteChanged {
		t.Fatalf("unrelated edit or reorder counted as consent: %+v", d)
	}
}

func TestConsentIsANewPatternForTheTargetRepo(t *testing.T) {
	e, _ := heldSetup(t)
	e.r.Replace(idFile(
		Rule{Remote: "github.com/old/x", Repo: "projects"},
		Rule{Remote: "github.com/new/x", Repo: "client-a"},
	))
	if d := e.r.Decide("-f"); d.Repo != "client-a" {
		t.Fatalf("a newly listed name is consent: %+v", d)
	}
}

func TestMoveOutOfClientNeedsAnExplicitName(t *testing.T) {
	f := idFile(
		Rule{Remote: "github.com/client/x", Repo: "client-a"},
		Rule{Remote: "github.com/new/**", Repo: "projects"},
	)
	e := newIDEnv(t, f)
	e.remotes["-c"], e.roots["-c"] = "github.com/client/x", []string{"bbb"}
	e.r.Decide("-c")
	e.remotes["-c"] = "github.com/new/x"
	e.r.Refresh()
	if d := e.r.Decide("-c"); d.Reason != ReasonRemoteChanged {
		t.Fatal(d)
	}
	// A new glob is not enough to take a client folder out.
	e.r.Replace(idFile(
		Rule{Remote: "github.com/client/x", Repo: "client-a"},
		Rule{Remote: "github.com/new/*", Repo: "projects"},
	))
	if d := e.r.Decide("-c"); d.Repo != "" {
		t.Fatalf("glob moved a client folder out: %+v", d)
	}
	// An alias naming the key is explicit.
	e.r.Replace(idFile(
		Rule{Remote: "github.com/client/x", Repo: "client-a"},
		Rule{Remote: "github.com/other/z", Aliases: []string{"github.com/new/x"}, Repo: "projects"},
	))
	if d := e.r.Decide("-c"); d.Repo != "projects" {
		t.Fatalf("explicit alias should consent: %+v", d)
	}
}

func TestTwoWritersKeepBothRecords(t *testing.T) {
	f := idFile(Rule{Remote: "github.com/o/**", Repo: "projects"})
	e := newIDEnv(t, f)
	a, b := e.router(f), e.router(f)
	for _, r := range []*Router{a, b} {
		r.mu.Lock()
		r.loadIDsLocked()
		r.mu.Unlock()
	}
	e.remotes["-a"], e.roots["-a"] = "github.com/o/a", []string{"1"}
	e.remotes["-b"], e.roots["-b"] = "github.com/o/b", []string{"2"}
	a.Decide("-a")
	b.Decide("-b")
	raw, _ := os.ReadFile(e.idFile())
	if !strings.Contains(string(raw), "github.com/o/a") || !strings.Contains(string(raw), "github.com/o/b") {
		t.Fatalf("a record was lost: %s", raw)
	}
}

func TestCorruptIdentityFileDeniesRecordedFoldersAndIsNeverOverwritten(t *testing.T) {
	f := idFile(Rule{Remote: "github.com/o/**", Repo: "projects"})
	e := newIDEnv(t, f)
	e.remotes["-f"], e.roots["-f"] = "github.com/o/f", []string{"1"}
	if d := e.r.Decide("-f"); d.Repo != "projects" {
		t.Fatal(d)
	}
	if err := os.WriteFile(e.idFile(), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	r2 := e.router(idFile(Rule{Remote: "github.com/o/**", Repo: "client-a"})) // would move it
	if d := r2.Decide("-f"); d.Repo != "" || d.Reason != ReasonIdentityUnreadable {
		t.Fatalf("recorded folder must be held: %+v", d)
	}
	r2 = e.router(f)
	if d := r2.Decide("-f"); d.Repo != "projects" {
		t.Fatalf("staying put must continue: %+v", d)
	}
	// A folder that was never recorded is not affected, and nothing is written.
	e.remotes["-new"], e.roots["-new"] = "github.com/o/n", []string{"3"}
	if d := r2.Decide("-new"); d.Repo != "projects" {
		t.Fatalf("new folder: %+v", d)
	}
	if raw, _ := os.ReadFile(e.idFile()); string(raw) != "{not json" {
		t.Fatalf("corrupt file overwritten: %s", raw)
	}
	if r2.IdentityProblem() == "" {
		t.Fatal("problem not reported")
	}
	if s := r2.Summarize([]string{"-f"}); !s.Unreadable {
		t.Fatalf("summary: %+v", s)
	}
}

func TestReadOnlyRouterWritesNothing(t *testing.T) {
	f := idFile(Rule{Remote: "github.com/o/**", Repo: "projects"})
	e := newIDEnv(t, f)
	r := e.router(f)
	r.ReadOnly = true // (New alone is read-only too; see TestNewIsReadOnlyByDefault)
	e.remotes["-f"], e.roots["-f"] = "github.com/o/f", []string{"1"}
	if d := r.Decide("-f"); d.Repo != "projects" {
		t.Fatal(d)
	}
	r.Summarize([]string{"-f"})
	for _, p := range []string{e.idFile(), e.state} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("%s written by a read-only router", p)
		}
	}
}

func TestWriteRetriesRenameAndSurfacesFailure(t *testing.T) {
	old, oldSleep := renameFn, retrySleep
	defer func() { renameFn, retrySleep = old, oldSleep }()
	retrySleep = func(time.Duration) {}
	n := 0
	renameFn = func(a, b string) error {
		n++
		if n < 3 {
			return fmt.Errorf("sharing violation")
		}
		return os.Rename(a, b)
	}
	p := filepath.Join(t.TempDir(), "x.json")
	if err := writeJSONAtomic(p, map[string]int{"a": 1}, ".x-*.tmp"); err != nil || n != 3 {
		t.Fatalf("err %v after %d tries", err, n)
	}
	renameFn = func(a, b string) error { return fmt.Errorf("denied") }
	if err := writeJSONAtomic(p, map[string]int{"a": 2}, ".x-*.tmp"); err == nil {
		t.Fatal("persistent failure not surfaced")
	}
	ents, _ := os.ReadDir(filepath.Dir(p))
	for _, en := range ents {
		if strings.HasSuffix(en.Name(), ".tmp") {
			t.Fatalf("temp file left: %s", en.Name())
		}
	}
}

func TestFallbackNotForPersonalOrMissingRepo(t *testing.T) {
	f := idFile(Rule{Remote: "github.com/old/x", Repo: "projects"})
	e := newIDEnv(t, f)
	e.remotes["-f"], e.roots["-f"] = "github.com/old/x", []string{"aaa"}
	e.r.Decide("-f")
	e.remotes["-f"] = "github.com/new/x"
	// The recorded repo became the personal repo, which owns no folders.
	g := idFile(Rule{Remote: "github.com/old/x", Repo: "client-a"})
	g.Personal = "projects"
	g.Repos["projects"] = Repo{Config: "/x", Kind: KindPersonal}
	g.Repos["personal"] = Repo{Config: "/y", Kind: KindProject}
	if d := e.router(g).Decide("-f"); d.Repo != "" {
		t.Fatalf("routed to the personal repo: %+v", d)
	}
	// The recorded repo is not on this host.
	e.have = func(n string) bool { return n != "projects" }
	if d := e.router(f).Decide("-f"); d.Repo != "" {
		t.Fatalf("routed to a repo this host lacks: %+v", d)
	}
}

// A repo of kind personal that the "personal" field does not name never gets
// the rename fallback either.
func TestFallbackNotForKindPersonalRepoNotNamedInPersonal(t *testing.T) {
	f := idFile(Rule{Remote: "github.com/old/x", Repo: "projects"})
	e := newIDEnv(t, f)
	e.remotes["-f"], e.roots["-f"] = "github.com/old/x", []string{"aaa"}
	e.r.Decide("-f")
	e.remotes["-f"] = "github.com/new/x"
	g := idFile(Rule{Remote: "github.com/old/x", Repo: "projects"})
	g.Personal = ""
	g.Repos["projects"] = Repo{Config: "/x", Kind: KindPersonal}
	if d := e.router(g).Decide("-f"); d.Repo != "" {
		t.Fatalf("renamed folder routed to a kind-personal repo: %+v", d)
	}
}

func TestNewListedFolderSharingRootsGetsNoFallback(t *testing.T) {
	f := idFile(Rule{Remote: "github.com/old/x", Repo: "projects"})
	e := newIDEnv(t, f)
	e.remotes["-a"], e.roots["-a"] = "github.com/old/x", []string{"aaa"}
	e.r.Decide("-a")
	e.remotes["-b"], e.roots["-b"] = "github.com/other/x", []string{"aaa"}
	// Record a different folder's identity under the name that sorts first, so a
	// lookup by anything but the folder itself would find a record.
	if d := e.r.Decide("-b"); d.Repo != "" || d.Note != "" {
		t.Fatalf("got %+v", d)
	}
}

func TestRoutesEditedAwayDeniesAtOnce(t *testing.T) {
	f := idFile(Rule{Remote: "github.com/old/x", Repo: "projects"})
	e := newIDEnv(t, f)
	e.remotes["-f"], e.roots["-f"] = "github.com/old/x", []string{"aaa"}
	e.r.Decide("-f")
	// Same key, but the user removed the rule: that is a decision, not a rename.
	e.r.Replace(idFile())
	if d := e.r.Decide("-f"); d.Repo != "" || d.Note != "" {
		t.Fatalf("got %+v", d)
	}
}

func TestRecordedRootsSurviveARenameToAnAlias(t *testing.T) {
	f := idFile(Rule{Remote: "github.com/old/x", Aliases: []string{"github.com/mid/x"}, Repo: "projects"})
	e := newIDEnv(t, f)
	e.remotes["-f"], e.roots["-f"] = "github.com/old/x", []string{"aaa"}
	e.r.Decide("-f")
	e.remotes["-f"] = "github.com/mid/x"
	delete(e.roots, "-f") // unreadable at this tick: the recorded roots must stay
	e.r.Refresh()
	e.r.Decide("-f")
	e.remotes["-f"], e.roots["-f"] = "github.com/new/x", []string{"aaa"}
	e.r.Refresh()
	if d := e.r.Decide("-f"); d.Repo != "projects" || d.Note != NoteRenamed {
		t.Fatalf("recorded roots were lost: %+v", d)
	}
}

func TestClientFolderTransferredToAnotherOwnerDeniesAtOnce(t *testing.T) {
	f := idFile(Rule{Remote: "github.com/client/x", Repo: "client-a"})
	e := newIDEnv(t, f)
	e.remotes["-c"], e.roots["-c"] = "github.com/client/x", []string{"bbb"}
	e.r.Decide("-c")
	e.remotes["-c"] = "github.com/someone-else/x"
	e.r.Refresh()
	if d := e.r.Decide("-c"); d.Repo != "" || d.Reason != ReasonOwnerChanged {
		t.Fatalf("got %+v", d)
	}
	e.remotes["-c"] = "github.com/client/x-renamed"
	e.r.Refresh()
	if d := e.r.Decide("-c"); d.Repo != "client-a" || d.Note != NoteRenamed {
		t.Fatalf("same-owner rename: %+v", d)
	}
	// A non-client folder may change owner within the grace period.
	e2 := newIDEnv(t, idFile(Rule{Remote: "github.com/old/x", Repo: "projects"}))
	e2.remotes["-p"], e2.roots["-p"] = "github.com/old/x", []string{"aaa"}
	e2.r.Decide("-p")
	e2.remotes["-p"] = "github.com/new/x"
	e2.r.Refresh()
	if d := e2.r.Decide("-p"); d.Repo != "projects" {
		t.Fatalf("non-client transfer: %+v", d)
	}
}

func TestRootsCachedPerCheckoutAndHead(t *testing.T) {
	r := New(idFile(), func(string) bool { return true })
	r.Candidates = func(string) []string { return []string{"/co"} }
	head, calls := "h1", map[string]int{}
	shallow := "false"
	r.Git = func(dir string, args ...string) (string, error) {
		calls[args[0]]++
		switch args[0] {
		case "rev-parse":
			if args[1] == "HEAD" {
				return head, nil
			}
			return shallow, nil
		}
		return "c2\nc1", nil
	}
	for i := 0; i < 3; i++ {
		rs, ok := r.defaultRoots("-f")
		if !ok || len(rs) != 2 || rs[0] != "c1" {
			t.Fatalf("roots %v %v", rs, ok)
		}
	}
	if calls["rev-list"] != 1 {
		t.Fatalf("rev-list ran %d times", calls["rev-list"])
	}
	head = "h2"
	r.defaultRoots("-f")
	if calls["rev-list"] != 2 {
		t.Fatalf("new HEAD must recompute, got %d", calls["rev-list"])
	}
	shallow, head = "true", "h3"
	for i := 0; i < 3; i++ {
		if _, ok := r.defaultRoots("-f"); ok {
			t.Fatal("shallow gave roots")
		}
	}
	shallowChecks := calls["rev-parse"]
	r.defaultRoots("-f")
	if calls["rev-parse"] != shallowChecks+1 { // only the HEAD lookup
		t.Fatalf("shallow result not cached until HEAD changes: %v", calls)
	}
}

func TestRenamedSinceClampedToNow(t *testing.T) {
	f := idFile(Rule{Remote: "github.com/old/x", Repo: "projects"})
	e := newIDEnv(t, f)
	e.remotes["-f"], e.roots["-f"] = "github.com/old/x", []string{"aaa"}
	e.r.Decide("-f")
	raw, _ := os.ReadFile(e.idFile())
	future := e.now.Add(400 * 24 * time.Hour).Unix()
	raw = []byte(strings.Replace(string(raw), `"roots"`, fmt.Sprintf(`"renamedSince": %d, "roots"`, future), 1))
	os.WriteFile(e.idFile(), raw, 0600)
	e.remotes["-f"] = "github.com/new/x"
	d := e.router(f).Decide("-f")
	if d.Repo != "projects" || d.GraceEnds.After(e.now.Add(31*24*time.Hour)) {
		t.Fatalf("future start not clamped: %+v", d)
	}
}

func TestGraceSurvivesKeyFlaps(t *testing.T) {
	f := idFile(
		Rule{Remote: "github.com/old/x", Repo: "projects"},
		Rule{Remote: "github.com/elsewhere/**", Repo: "client-a"},
	)
	e := newIDEnv(t, f)
	e.remotes["-f"], e.roots["-f"] = "github.com/old/x", []string{"aaa"}
	e.r.Decide("-f")
	step := func(key string, days int) Decision {
		e.day(days)
		e.remotes["-f"] = key
		e.r.Refresh()
		return e.r.Decide("-f")
	}
	if d := step("github.com/new/x", 0); d.Note != NoteRenamed {
		t.Fatal(d)
	}
	step("github.com/elsewhere/x", 10) // routed to another repo: held
	if d := step("github.com/newer/x", 10); d.Repo != "projects" {
		t.Fatalf("day 20: %+v", d)
	}
	if d := step("github.com/new/x", 12); d.Repo != "" || d.Reason != ReasonRenameExpired {
		t.Fatalf("flapping restarted the clock: %+v", d)
	}
	// A rule matching again (the old name) ends the state.
	if d := step("github.com/old/x", 0); d.Repo != "projects" || d.Note != "" {
		t.Fatal(d)
	}
	if d := step("github.com/new/x", 0); d.Note != NoteRenamed || !d.GraceEnds.After(e.now) {
		t.Fatalf("fresh grace expected: %+v", d)
	}
}

func TestDifferentRepositoryAtThePath(t *testing.T) {
	f := idFile(Rule{Remote: "github.com/old/x", Repo: "projects"})
	e := newIDEnv(t, f)
	e.remotes["-f"], e.roots["-f"] = "github.com/old/x", []string{"aaa"}
	e.r.Decide("-f")
	e.remotes["-f"], e.roots["-f"] = "github.com/new/x", []string{"zzz"}
	e.r.Refresh()
	d := e.r.Decide("-f")
	if d.Repo != "" || d.Reason != ReasonDifferentRepo {
		t.Fatalf("got %+v", d)
	}
	if s := e.r.Summarize([]string{"-f"}); len(s.Different) != 1 {
		t.Fatalf("summary %+v", s)
	}
}

func TestFileCorruptedWhileRunningIsNotOverwritten(t *testing.T) {
	f := idFile(Rule{Remote: "github.com/o/**", Repo: "projects"})
	e := newIDEnv(t, f)
	e.remotes["-a"], e.roots["-a"] = "github.com/o/a", []string{"1"}
	e.r.Decide("-a")
	if err := os.WriteFile(e.idFile(), []byte("garbage"), 0600); err != nil {
		t.Fatal(err)
	}
	e.remotes["-b"], e.roots["-b"] = "github.com/o/b", []string{"2"}
	e.r.Decide("-b")
	if raw, _ := os.ReadFile(e.idFile()); string(raw) != "garbage" {
		t.Fatalf("overwritten: %s", raw)
	}
	if e.r.IdentityProblem() == "" {
		t.Fatal("not reported")
	}
}

func TestNewIsReadOnlyByDefault(t *testing.T) {
	if r := New(File{}, func(string) bool { return true }); !r.ReadOnly {
		t.Fatal("route.New must default to read-only")
	}
	r := New(File{}, func(string) bool { return true })
	r.Writable()
	if r.ReadOnly {
		t.Fatal("Writable did not take effect")
	}
}

func TestProbeCorruptMidSaveRecovers(t *testing.T) {
	f := idFile(Rule{Remote: "github.com/old/x", Repo: "projects"}, Rule{Remote: "github.com/old/y", Repo: "projects"})
	e := newIDEnv(t, f)
	p := e.idFile()
	e.remotes["-a"], e.roots["-a"] = "github.com/old/x", []string{"aaa"}
	e.r.Decide("-a")
	e.remotes["-b"], e.roots["-b"] = "github.com/old/y", []string{"bbb"}
	e.r.afterCompute = func() { os.WriteFile(p, []byte("{garbage"), 0600) }
	e.r.Decide("-b")
	e.r.afterCompute = nil
	os.Remove(p) // the user fixes it by removing it
	e.r.Refresh()
	if d := e.r.Decide("-a"); d.Repo != "projects" {
		t.Fatalf("wedged after fix: %+v problem=%q", d, e.r.IdentityProblem())
	}
	if e.r.IdentityProblem() != "" {
		t.Fatal("problem still reported")
	}
	// The entry that was waiting to be saved is merged into the new file.
	if raw, _ := os.ReadFile(p); !strings.Contains(string(raw), "github.com/old/y") {
		t.Fatalf("pending record lost: %s", raw)
	}
}

func TestUnreadableDeniesOnlyWouldBeMoves(t *testing.T) {
	f := idFile(Rule{Remote: "github.com/old/x", Repo: "projects"})
	e := newIDEnv(t, f)
	e.remotes["-a"], e.roots["-a"] = "github.com/old/x", []string{"aaa"}
	e.r.Decide("-a")
	os.WriteFile(e.idFile(), []byte("{garbage"), 0600)
	e.r.Refresh()
	if d := e.r.Decide("-a"); d.Repo != "projects" {
		t.Fatalf("unrenamed folder denied: %+v", d)
	}
	// A route edit that would move the folder is held while the file is bad.
	g := idFile(Rule{Remote: "github.com/old/x", Repo: "client-a"})
	if d := e.router(g).Decide("-a"); d.Repo != "" || d.Reason != ReasonIdentityUnreadable {
		t.Fatalf("move not held: %+v", d)
	}
}

// With the key unchanged a routes edit or reorder is the user's doing, as it
// was before root-commit identities: the folder moves, and a move out of a
// client repo is logged.
func TestSameKeyRetargetMovesAndLogs(t *testing.T) {
	f := idFile(
		Rule{Remote: "github.com/client/**", Repo: "client-a"},
		Rule{Remote: "github.com/client/x", Repo: "projects"},
	)
	e := newIDEnv(t, f)
	e.remotes["-c"], e.roots["-c"] = "github.com/client/x", []string{"bbb"}
	if d := e.r.Decide("-c"); d.Repo != "client-a" {
		t.Fatal(d)
	}
	e.log.Reset()
	e.r.Replace(idFile( // reordered: the older, narrower rule now wins
		Rule{Remote: "github.com/client/x", Repo: "projects"},
		Rule{Remote: "github.com/client/**", Repo: "client-a"},
	))
	if d := e.r.Decide("-c"); d.Repo != "projects" {
		t.Fatalf("same-key retarget was held: %+v", d)
	}
	if !strings.Contains(e.log.String(), "moved from client repo") {
		t.Fatalf("log: %q", e.log.String())
	}
}
