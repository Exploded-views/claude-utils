package route

import (
	"bytes"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testRouter builds a Router whose remote resolution is a lookup table, so the
// matching tests need no git. have names the repos present on the host.
func testRouter(t *testing.T, f File, have []string, remotes map[string]string) *Router {
	t.Helper()
	present := map[string]bool{}
	for _, h := range have {
		present[h] = true
	}
	r := New(f, func(name string) bool { return present[name] })
	r.Resolve = func(folder string) (string, resolution) {
		if k, ok := remotes[folder]; ok {
			if k == "?" {
				return "", resolutionAmbiguous
			}
			return k, resolutionOK
		}
		return "", resolutionNone
	}
	r.Logf = func(string, ...any) {}
	return r
}

func fileOf(routes ...Rule) File {
	return File{
		Repos: map[string]Repo{
			"personal": {Config: "/x/personal/config.json"},
			"projects": {Config: "/x/projects/config.json"},
			"client-a": {Config: "/x/client-a/config.json", Client: true},
		},
		Personal: "personal",
		Routes:   routes,
	}
}

func TestUnmatchedFolderDenied(t *testing.T) {
	r := testRouter(t, fileOf(Rule{Folder: "-home-me-known", Repo: "projects"}),
		[]string{"personal", "projects", "client-a"}, nil)
	if d := r.Decide("-home-me-other"); d.Repo != "" {
		t.Fatalf("unmatched folder routed to %q", d.Repo)
	}
	if d := r.Decide("-home-me-known"); d.Repo != "projects" {
		t.Fatalf("folder rule: got %q", d.Repo)
	}
}

func TestRouteToMissingRepoDenied(t *testing.T) {
	f := fileOf(
		Rule{Remote: "github.com/client/*", Repo: "client-a"},
		Rule{Remote: "github.com/<you>/*", Repo: "projects"},
	)
	// client-a is not on this host: its rule must deny, not fall through.
	r := testRouter(t, f, []string{"personal", "projects"}, map[string]string{
		"-w-c": "github.com/client/app",
	})
	d := r.Decide("-w-c")
	if d.Repo != "" || d.Reason != ReasonRepoMissing {
		t.Fatalf("got %+v, want denied (repo missing)", d)
	}
}

func TestMissingRepoRuleDoesNotFallThroughToBroaderRule(t *testing.T) {
	f := fileOf(
		Rule{Remote: "github.com/me/secret", Repo: "client-a"},
		Rule{Remote: "github.com/me/*", Repo: "projects"},
	)
	r := testRouter(t, f, []string{"personal", "projects"}, map[string]string{"-f": "github.com/me/secret"})
	if d := r.Decide("-f"); d.Repo != "" {
		t.Fatalf("fell through to %q", d.Repo)
	}
}

func TestFirstMatchWinsAndShadowing(t *testing.T) {
	f := fileOf(
		Rule{Remote: "github.com/me/*", Repo: "projects"},
		Rule{Remote: "github.com/me/special", Repo: "client-a"}, // shadowed
		Rule{Folder: "-a", Repo: "projects"},
		Rule{Folder: "-a", Repo: "client-a"}, // shadowed (identical)
	)
	r := testRouter(t, f, []string{"personal", "projects", "client-a"}, map[string]string{"-s": "github.com/me/special"})
	if d := r.Decide("-s"); d.Repo != "projects" || d.Rule != 0 {
		t.Fatalf("got %+v", d)
	}
	sh := r.Shadowed()
	got := map[int]int{}
	for _, s := range sh {
		got[s.Rule] = s.By
	}
	if got[1] != 0 || got[3] != 2 || len(got) != 2 {
		t.Fatalf("shadowed = %v", sh)
	}
}

func TestRemoteKeyMatching(t *testing.T) {
	cases := []struct {
		pat, key string
		want     bool
	}{
		{"github.com/me/*", "github.com/me/app", true},
		{"github.com/me/*", "github.com/me/app/sub", false}, // * is one segment
		{"github.com/me/**", "github.com/me/app/sub", true},
		{"github.com/me/app", "github.com/me/app", true},
		{"github.com/me/app", "github.com/me/app2", false},
		{"gitlab.example.internal/**", "gitlab.example.internal/g/sub/r", true},
		{"*", "github.com/me/app", false},
		{"github.com/me/infra-*", "github.com/me/infra-home", true},
		{"github.com/me/infra-*", "github.com/me/app", false},
	}
	for _, c := range cases {
		if got := globMatch(c.pat, c.key); got != c.want {
			t.Errorf("globMatch(%q,%q)=%v want %v", c.pat, c.key, got, c.want)
		}
	}
}

func TestNormalizeRemote(t *testing.T) {
	same := []string{
		"git@github.com:Me/App.git",
		"https://github.com/me/app",
		"https://user:tok@github.com/ME/app.git/",
		"ssh://git@github.com:22/me/app.git",
	}
	for _, u := range same {
		if k, ok := NormalizeRemote(u); !ok || k != "github.com/me/app" {
			t.Errorf("%q -> %q,%v", u, k, ok)
		}
	}
	for _, u := range []string{"", "/srv/git/app.git", "file:///x/y", "C:\\x\\y", "https://github.com/app"} {
		if k, ok := NormalizeRemote(u); ok {
			t.Errorf("%q should not normalise, got %q", u, k)
		}
	}
}

func TestPathRuleAndAmbiguousRemote(t *testing.T) {
	f := fileOf(
		Rule{Remote: "github.com/me/*", Repo: "projects"},
		Rule{Folder: "-pinned", Repo: "projects"},
	)
	r := testRouter(t, f, []string{"personal", "projects", "client-a"}, map[string]string{"-amb": "?", "-pinned": "?"})
	if d := r.Decide("-amb"); d.Repo != "" || d.Reason != ReasonUnresolved {
		t.Fatalf("ambiguous folder: %+v", d)
	}
	if d := r.Decide("-pinned"); d.Repo != "projects" {
		t.Fatalf("pinned folder rule should win over ambiguity: %+v", d)
	}
}

func TestPathGlobAgainstRealCandidates(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "clients", "a", "app")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	f := fileOf(
		Rule{Path: filepath.ToSlash(filepath.Join(root, "clients", "a")) + "/**", Repo: "client-a"},
		Rule{Path: filepath.ToSlash(root) + "/**", Repo: "projects"},
	)
	r := New(f, func(string) bool { return true })
	r.Logf = func(string, ...any) {}
	if d := r.Decide(escape(dir)); d.Repo != "client-a" {
		t.Fatalf("path rule: %+v", d)
	}
	other := filepath.Join(root, "mine")
	_ = os.MkdirAll(other, 0700)
	if d := r.Decide(escape(other)); d.Repo != "projects" {
		t.Fatalf("second path rule: %+v", d)
	}
	if d := r.Decide(escape(filepath.Join(root, "gone"))); d.Repo != "" {
		t.Fatalf("absent folder must be denied: %+v", d)
	}
}

func TestRemoteFromGitOrigin(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	dir := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", "git@github.com:Me/Thing.git"},
	} {
		c := exec.Command("git", args...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	f := fileOf(Rule{Remote: "github.com/me/*", Repo: "projects"})
	r := New(f, func(string) bool { return true })
	r.Logf = func(string, ...any) {}
	if d := r.Decide(escape(dir)); d.Repo != "projects" {
		t.Fatalf("origin-based route: %+v", d)
	}
}

func TestDeniedLoggedOnceByHashNotName(t *testing.T) {
	var buf bytes.Buffer
	r := testRouter(t, fileOf(), []string{"personal"}, nil)
	r.Logf = func(f string, a ...any) { log.New(&buf, "", 0).Printf(f, a...) }
	for i := 0; i < 3; i++ {
		r.Decide("-secret-client-folder")
		r.Refresh()
	}
	out := buf.String()
	if n := strings.Count(out, "denied"); n != 1 {
		t.Fatalf("logged %d times: %q", n, out)
	}
	if strings.Contains(out, "secret-client-folder") {
		t.Fatalf("folder name leaked in log: %q", out)
	}
}

func TestMoveFromClientToNonClientLogged(t *testing.T) {
	state := filepath.Join(t.TempDir(), "owners.json")
	var buf bytes.Buffer
	logf := func(f string, a ...any) { log.New(&buf, "", 0).Printf(f, a...) }

	f1 := fileOf(Rule{Folder: "-w-x", Repo: "client-a"})
	r1 := New(f1, func(string) bool { return true })
	r1.StatePath, r1.Logf = state, logf
	if d := r1.Decide("-w-x"); d.Repo != "client-a" {
		t.Fatal(d)
	}
	buf.Reset()

	f2 := fileOf(Rule{Folder: "-w-x", Repo: "projects"})
	r2 := New(f2, func(string) bool { return true })
	r2.StatePath, r2.Logf = state, logf
	if d := r2.Decide("-w-x"); d.Repo != "projects" {
		t.Fatal(d)
	}
	if !strings.Contains(buf.String(), "moved from client repo") || strings.Contains(buf.String(), "-w-x") {
		t.Fatalf("log: %q", buf.String())
	}
}

func TestLoadValidation(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "r.json")
		if err := os.WriteFile(p, []byte(s), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	bad := map[string]string{
		"two matchers":   `{"repos":{"a":"/c"},"routes":[{"folder":"x","remote":"y","repo":"a"}]}`,
		"no matcher":     `{"repos":{"a":"/c"},"routes":[{"repo":"a"}]}`,
		"allow default":  `{"repos":{"a":"/c"},"default":"allow"}`,
		"personal route": `{"repos":{"a":"/c"},"personal":"a","routes":[{"folder":"x","repo":"a"}]}`,
		"junk":           `{`,
	}
	for name, s := range bad {
		if _, err := Load(write(s)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	// "unknown repo" is about the repos map; a rule naming a repo not present
	// on this host is fine at load time only if it appears in repos.
	f, err := Load(write(`{"repos":{"a":"/c","b":{"config":"/d","client":true}},"routes":[{"folder":"x","repo":"b"}],"default":"deny"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !f.Repos["b"].Client || f.Repos["a"].Config != "/c" {
		t.Fatalf("%+v", f.Repos)
	}
}

func TestSummarize(t *testing.T) {
	f := fileOf(
		Rule{Folder: "-p1", Repo: "projects"},
		Rule{Folder: "-c1", Repo: "client-a"},
	)
	r := testRouter(t, f, []string{"personal", "projects", "client-a"}, nil)
	s := r.Summarize([]string{"-p1", "-c1", "-zz", "-yy"})
	if s.Owned["projects"] != 1 || s.Owned["client-a"] != 1 || s.Owned["personal"] != 0 || s.Denied != 2 {
		t.Fatalf("%+v", s)
	}
}

func TestOwnsPersonalIsAlwaysFalse(t *testing.T) {
	r := testRouter(t, fileOf(Rule{Folder: "-a", Repo: "projects"}), []string{"personal", "projects"}, nil)
	if r.Owns("personal")("-a") || !r.Owns("projects")("-a") || r.Owns("client-a")("-a") {
		t.Fatal("Owns wrong")
	}
}
