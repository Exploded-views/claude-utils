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
	r := New(f, func(string) bool { return true })
	r.StatePath = e.state
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
	e3.remotes["-c"] = "github.com/moved/x"
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
