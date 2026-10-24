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
			"personal": {Config: "/x/personal/config.json", Kind: KindPersonal},
			"projects": {Config: "/x/projects/config.json", Kind: KindProject},
			"client-a": {Config: "/x/client-a/config.json", Kind: KindClient},
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
		Rule{Folder: "-pinned", Repo: "projects"}, // a pin goes before the rules that need a remote
		Rule{Remote: "github.com/me/*", Repo: "projects"},
		Rule{Folder: "-late", Repo: "projects"},
	)
	r := testRouter(t, f, []string{"personal", "projects", "client-a"}, map[string]string{"-amb": "?", "-pinned": "?", "-late": "?"})
	// An unresolved remote denies at once: the folder rule behind it is not
	// consulted.
	if d := r.Decide("-late"); d.Repo != "" || d.Reason != ReasonUnresolved {
		t.Fatalf("unresolved remote fell through: %+v", d)
	}
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
	t.Setenv("HOME", t.TempDir())
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
		"two matchers":     `{"repos":{"a":{"config":"/c","kind":"project"}},"routes":[{"folder":"x","remote":"y","repo":"a"}]}`,
		"no matcher":       `{"repos":{"a":{"config":"/c","kind":"project"}},"routes":[{"repo":"a"}]}`,
		"allow default":    `{"repos":{"a":{"config":"/c","kind":"project"}},"default":"allow"}`,
		"personal route":   `{"repos":{"a":{"config":"/c","kind":"project"}},"personal":"a","routes":[{"folder":"x","repo":"a"}]}`,
		"junk":             `{`,
		"bare string repo": `{"repos":{"a":"/c"}}`,
		"no kind":          `{"repos":{"a":{"config":"/c"}}}`,
		"unknown kind":     `{"repos":{"a":{"config":"/c","kind":"clinet"}}}`,
		"personal kind":    `{"repos":{"a":{"config":"/c","kind":"project"}},"personal":"a"}`,
	}
	for name, s := range bad {
		if _, err := Load(write(s)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	// "unknown repo" is about the repos map; a rule naming a repo not present
	// on this host is fine at load time only if it appears in repos.
	f, err := Load(write(`{"repos":{"a":{"config":"/c","kind":"project"},"b":{"config":"/d","kind":"client"}},"routes":[{"folder":"x","repo":"b"}],"default":"deny"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !f.Repos["b"].IsClient() || f.Repos["a"].IsClient() || f.Repos["a"].Config != "/c" {
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

// gitIn runs git in dir for the fixtures below.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func gitEnv(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
}

func clientRepo(t *testing.T, root, remote string) string {
	t.Helper()
	dir := filepath.Join(root, "clientrepo")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "init", "-q")
	gitIn(t, dir, "remote", "add", "origin", remote)
	if err := os.WriteFile(filepath.Join(dir, "sub", "f"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", "c")
	return dir
}

func broadRouter(root string) *Router {
	f := fileOf(
		Rule{Remote: "github.com/client-org/**", Repo: "client-a"},
		Rule{Path: filepath.ToSlash(root) + "/**", Repo: "projects"},
	)
	r := New(f, func(string) bool { return true })
	r.Logf = func(string, ...any) {}
	r.SSHHost = func(h string) string { return h }
	return r
}

// A linked worktree and a subdirectory of a client checkout belong to the
// client's repo, not to the broad path rule behind it.
func TestWorktreeAndSubdirOfClientRepoRouteToClient(t *testing.T) {
	gitEnv(t)
	root := t.TempDir()
	repo := clientRepo(t, root, "git@github.com:Client-Org/app.git")
	wt := filepath.Join(root, "wt")
	gitIn(t, repo, "worktree", "add", "-q", wt)
	r := broadRouter(root)
	for name, dir := range map[string]string{"top": repo, "subdir": filepath.Join(repo, "sub"), "worktree": wt, "worktree subdir": filepath.Join(wt, "sub")} {
		if d := r.Decide(escape(dir)); d.Repo != "client-a" {
			t.Errorf("%s: routed to %q (%s), want client-a", name, d.Repo, d.Reason)
		}
	}
}

// A folder whose remote cannot be found must not be caught by the broad path
// rule that follows the remote rule.
func TestUnresolvableRemoteAheadOfBroadPathRuleDenies(t *testing.T) {
	gitEnv(t)
	root := t.TempDir()
	plain := filepath.Join(root, "plain")
	noOrigin := filepath.Join(root, "noorigin")
	for _, d := range []string{plain, noOrigin} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, noOrigin, "init", "-q")
	r := broadRouter(root)
	for name, dir := range map[string]string{"not a repo": plain, "no origin": noOrigin, "absent": filepath.Join(root, "gone")} {
		if d := r.Decide(escape(dir)); d.Repo != "" || d.Reason != ReasonUnresolved {
			t.Errorf("%s: %+v, want denied (unresolved)", name, d)
		}
	}
}

func TestInsteadOfAndSSHAliasResolveToRealHost(t *testing.T) {
	gitEnv(t)
	root := t.TempDir()
	repo := clientRepo(t, root, "git@client-alias:Client-Org/app.git")
	r := broadRouter(root)
	r.SSHHost = func(h string) string {
		if h == "client-alias" {
			return "github.com"
		}
		return h
	}
	if d := r.Decide(escape(repo)); d.Repo != "client-a" {
		t.Fatalf("ssh alias: %+v", d)
	}
	// url.<base>.insteadOf, applied by git remote get-url.
	other := filepath.Join(root, "other")
	if err := os.MkdirAll(other, 0700); err != nil {
		t.Fatal(err)
	}
	gitIn(t, other, "init", "-q")
	gitIn(t, other, "config", "url.git@github.com:Client-Org/.insteadOf", "co:")
	gitIn(t, other, "remote", "add", "origin", "co:thing.git")
	if d := r.Decide(escape(other)); d.Repo != "client-a" {
		t.Fatalf("insteadOf: %+v", d)
	}
}

func TestRewriteSSHHost(t *testing.T) {
	look := func(h string) string {
		if h == "alias" {
			return "real.example"
		}
		return ""
	}
	for in, want := range map[string]string{
		"git@alias:o/r.git":        "git@real.example:o/r.git",
		"ssh://git@alias:2222/o/r": "ssh://git@real.example:2222/o/r",
		"https://alias/o/r":        "https://alias/o/r",
		"git@other:o/r.git":        "git@other:o/r.git",
	} {
		if got := rewriteSSHHost(in, look); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestRemotePatternsLowerCased(t *testing.T) {
	f := fileOf(Rule{Remote: "GitHub.com/Me/*", Repo: "projects"})
	r := testRouter(t, f, []string{"projects", "personal"}, map[string]string{"-x": "github.com/me/app"})
	if d := r.Decide("-x"); d.Repo != "projects" {
		t.Fatalf("mixed-case pattern did not match: %+v", d)
	}
}

func TestPathAndFolderGlobsFoldCaseOnCaseInsensitiveHosts(t *testing.T) {
	old := foldCase
	defer func() { foldCase = old }()
	foldCase = true
	f := fileOf(Rule{Folder: "-Home-Me-App", Repo: "projects"})
	r := testRouter(t, f, []string{"projects", "personal"}, nil)
	if d := r.Decide("-home-me-app"); d.Repo != "projects" {
		t.Fatalf("folded: %+v", d)
	}
	foldCase = false
	r2 := testRouter(t, f, []string{"projects", "personal"}, nil)
	if d := r2.Decide("-home-me-app"); d.Repo != "" {
		t.Fatalf("case-sensitive host matched: %+v", d)
	}
}

func TestReplaceSwapsTableAndClearsCache(t *testing.T) {
	r := testRouter(t, fileOf(Rule{Folder: "-a", Repo: "projects"}), []string{"projects", "personal"}, nil)
	if r.Decide("-a").Repo != "projects" {
		t.Fatal("setup")
	}
	r.Replace(fileOf())
	if r.Decide("-a").Repo != "" {
		t.Fatal("old decision survived Replace")
	}
}

func TestOwnersFileUsesUniqueTempAndLeavesNone(t *testing.T) {
	dir := t.TempDir()
	r := New(fileOf(Rule{Folder: "-w", Repo: "projects"}), func(string) bool { return true })
	r.StatePath, r.Logf = filepath.Join(dir, "owners.json"), func(string, ...any) {}
	r.Decide("-w")
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "owners.json" {
		t.Fatalf("dir holds %v", entries)
	}
}

func TestNonClient(t *testing.T) {
	f := fileOf(Rule{Folder: "-p", Repo: "projects"}, Rule{Folder: "-c", Repo: "client-a"})
	r := testRouter(t, f, []string{"personal", "projects", "client-a"}, nil)
	if !r.NonClient("-p") || r.NonClient("-c") || r.NonClient("-zz") {
		t.Fatal("NonClient wrong")
	}
}

func TestNonClientIsAllowListOfKinds(t *testing.T) {
	for kind, want := range map[string]bool{KindPersonal: true, KindProject: true, KindInfra: true, KindClient: false, KindOrch: false} {
		f := File{Repos: map[string]Repo{"r": {Config: "/c", Kind: kind}}, Routes: []Rule{{Folder: "-a", Repo: "r"}}}
		r := New(f, func(string) bool { return true })
		r.Logf = func(string, ...any) {}
		if got := r.NonClient("-a"); got != want {
			t.Errorf("kind %s: NonClient=%v want %v", kind, got, want)
		}
	}
	// A repo with no kind (a table built by hand) is never non-client.
	f := File{Repos: map[string]Repo{"r": {Config: "/c"}}, Routes: []Rule{{Folder: "-a", Repo: "r"}}}
	r := New(f, func(string) bool { return true })
	r.Logf = func(string, ...any) {}
	if r.NonClient("-a") {
		t.Error("an unclassified repo counted as non-client")
	}
}

// A decision computed against the old table must not be cached after the
// table was replaced.
func TestDecideDoesNotCacheAcrossReplace(t *testing.T) {
	r := testRouter(t, fileOf(Rule{Folder: "-a", Repo: "projects"}), []string{"projects", "personal"}, nil)
	swapped := false
	r.afterCompute = func() {
		if !swapped {
			swapped = true
			r.Replace(fileOf())
		}
	}
	if d := r.Decide("-a"); d.Repo != "projects" {
		t.Fatalf("setup: %+v", d)
	}
	if d := r.Decide("-a"); d.Repo != "" {
		t.Fatalf("stale decision cached after Replace: %+v", d)
	}
}

func TestSSHHostRefusesOptionLookingHosts(t *testing.T) {
	if got := sshHostname("-oProxyCommand=touch /tmp/x"); got != "" {
		t.Fatalf("got %q", got)
	}
}

// Inherited GIT_* variables must not steer remote resolution.
func TestRemoteResolutionIgnoresInheritedGitEnv(t *testing.T) {
	gitEnv(t)
	root := t.TempDir()
	repo := clientRepo(t, root, "git@github.com:Client-Org/app.git")
	t.Setenv("GIT_DIR", filepath.Join(root, "nowhere"))
	t.Setenv("GIT_WORK_TREE", root)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "remote.origin.url")
	t.Setenv("GIT_CONFIG_VALUE_0", "git@github.com:me/mine.git")
	if d := broadRouter(root).Decide(escape(repo)); d.Repo != "client-a" {
		t.Fatalf("env steered resolution: %+v", d)
	}
}
