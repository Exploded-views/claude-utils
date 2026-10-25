package route

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func hostFile() File {
	return File{
		Repos: map[string]Repo{
			"personal": {Config: "/home/zed/.claudesync.d/personal/config.json", Kind: KindPersonal},
			"projects": {Config: "/home/zed/.claudesync.d/projects/config.json", Kind: KindProject},
			"orch-a":   {Config: "/home/zed/.claudesync.d/orch-a/config.json", Kind: KindOrch},
			"lab":      {Config: "/home/zed/.claudesync.d/lab/config.json", Kind: KindInfra},
		},
		Personal: "personal",
		Routes: []Rule{
			{Folder: "-home-zed", Repo: "lab"},
			{Path: "/home/zed/clients/acme/**", Repo: "orch-a"},
			{Path: "/home/zed/lab/**", Repo: "lab"},
			{Folder: "-home-zed-scratch", Repo: "projects"},
			{Remote: "github.com/acme/**", Aliases: []string{"github.com/acme-old/**"}, Repo: "orch-a"},
			{Remote: "github.com/zed/*", Repo: "projects"},
		},
		Registry: "/home/zed/registry",
	}
}

func TestValidLabel(t *testing.T) {
	for _, ok := range []string{"laptop-2", "a", "0x", strings.Repeat("a", 32)} {
		if err := ValidLabel(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-a", "A", "a_b", "a b", "../x", "a/b", strings.Repeat("a", 33), "a.b", "a\n"} {
		if err := ValidLabel(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestTemplateStripsPathsKeysHomeAndClients(t *testing.T) {
	tpl := hostFile().Template("-home-zed", "/home/zed")
	b, _ := json.Marshal(tpl)
	s := string(b)
	for _, leak := range []string{"/home/zed", "-home-zed", "config", ".claudesync", "registry", "://", "@", "orch-a", "acme"} {
		if strings.Contains(s, leak) {
			t.Errorf("template contains %q: %s", leak, s)
		}
	}
	if len(tpl.Repos) != 3 || tpl.Repos["lab"] != KindInfra {
		t.Fatalf("repos: %v", tpl.Repos)
	}
	// Dropped: the orch path rule and the orch remote rule. Left: home, lab
	// path, scratch pin, projects remote.
	if len(tpl.Routes) != 4 || !tpl.Routes[0].Home || tpl.Routes[0].Repo != "lab" || tpl.Routes[0].Folder != "" {
		t.Fatalf("routes: %+v", tpl.Routes)
	}
	if tpl.Routes[1].Path != "{home}/lab/**" || tpl.Routes[2].Folder != "{home}-scratch" {
		t.Fatalf("home not replaced: %+v", tpl.Routes)
	}
}

func TestTemplatePlaceholderFallbackForOtherUsers(t *testing.T) {
	f := hostFile()
	f.Routes = []Rule{{Folder: "-Users-someoneelse-x", Repo: "projects"}, {Path: "/Users/someoneelse/x/**", Repo: "projects"}}
	tpl := f.Template("-home-zed", "/home/zed")
	if tpl.Routes[0].Folder != "{home}-x" || tpl.Routes[1].Path != "{home}/x/**" {
		t.Fatalf("%+v", tpl.Routes)
	}
}

func laptop() File {
	return File{
		Repos: map[string]Repo{
			"personal": {Config: "/Users/zed/p/config.json", Kind: KindPersonal},
			"projects": {Config: "/Users/zed/pr/config.json", Kind: KindProject},
		},
		Personal: "personal",
		Routes:   []Rule{{Folder: "-Users-zed-old", Repo: "projects"}},
		Registry: "/Users/zed/registry",
	}
}

// clientTpl is a template as an older version may have published it, with
// client rules in it.
func clientTpl() Template {
	return Template{Version: 1,
		Repos: map[string]string{"projects": KindProject, "client-a": KindClient, "lab": KindInfra},
		Routes: []TemplateRule{
			{Folder: "{home}-acme*", Repo: "client-a"},
			{Path: "{home}/acme/**", Repo: "client-a"},
			{Remote: "github.com/acme/**", Repo: "client-a"},
			{Folder: "{home}-*", Repo: "projects"},
			{Remote: "github.com/zed/*", Repo: "projects"},
			{Path: "{home}/lab/**", Repo: "lab"},
		}}
}

func TestInheritKeepsDeniesForDroppedRulesAndExpandsHome(t *testing.T) {
	g, rep, err := clientTpl().Inherit(laptop(), "-Users-zed", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Dropped) != 4 {
		t.Fatalf("dropped: %v", rep.Dropped)
	}
	for _, r := range g.Routes {
		if r.Repo == "client-a" || r.Repo == "lab" {
			t.Fatalf("route to a repo the host lacks: %+v", r)
		}
	}
	r := testRouter(t, g, []string{"personal", "projects"}, map[string]string{"-Users-zed-other": "github.com/other/x"})
	r.Candidates = func(string) []string { return []string{"/nonexistent/x"} }
	if d := r.Decide("-Users-zed-acme-notes"); d.Repo != "" {
		t.Fatalf("client folder captured by the broader rule: %+v", d)
	}
	if d := r.Decide("-Users-zed-other"); d.Repo != "projects" {
		t.Fatalf("expansion of {home}: %+v", d)
	}
	for _, r := range g.Routes {
		if strings.Contains(r.Folder+r.Path, "{home}") {
			t.Fatalf("placeholder left: %+v", r)
		}
	}
	// Host settings stay the host's own.
	if g.Repos["projects"].Config != "/Users/zed/pr/config.json" || g.Registry != "/Users/zed/registry" || g.Personal != "personal" {
		t.Fatalf("host data changed: %+v", g)
	}
}

func TestInheritKindMismatchDeniedNotRouted(t *testing.T) {
	cur := laptop()
	cur.Repos["client-a"] = Repo{Config: "/x", Kind: KindProject} // same name, other kind
	g, rep, err := clientTpl().Inherit(cur, "-Users-zed", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range g.Routes {
		if r.Repo == "client-a" {
			t.Fatalf("kept a rule whose repo has another kind here: %+v", r)
		}
	}
	if !strings.Contains(strings.Join(rep.Dropped, "\n"), "kind") {
		t.Fatalf("dropped: %v", rep.Dropped)
	}
}

func TestInheritHomePinOnlyWithHomeFlag(t *testing.T) {
	cur := laptop()
	cur.Repos["lab"] = Repo{Config: "/x", Kind: KindInfra}
	cur.Routes = append([]Rule{{Folder: "-Users-zed", Repo: "personal"}}, cur.Routes...)
	tpl := Template{Version: 1, Repos: map[string]string{"lab": KindInfra}, Routes: []TemplateRule{{Home: true, Repo: "lab"}}}

	g, _, err := tpl.Inherit(cur, "-Users-zed", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := g.HomePin("-Users-zed"); !ok || r != "personal" {
		t.Fatalf("own home pin not kept: %q %v", r, ok)
	}
	g, _, err = tpl.Inherit(cur, "-Users-zed", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := g.HomePin("-Users-zed"); !ok || r != "lab" || g.Routes[0].Folder != "-Users-zed" {
		t.Fatalf("template home pin not applied on top: %q %+v", r, g.Routes)
	}
}

func TestInheritPersonalNeedsPin(t *testing.T) {
	tpl := Template{Version: 1, Repos: map[string]string{"personal": KindPersonal},
		Routes: []TemplateRule{{Remote: "github.com/zed/*", Repo: "personal"}, {Home: true, Repo: "personal"}}}
	g, rep, err := tpl.Inherit(laptop(), "-Users-zed", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Routes) != 2 || g.Routes[0].Folder != "-Users-zed" || g.Routes[1].Repo == "personal" || len(rep.Dropped) != 1 {
		t.Fatalf("%+v %v", g.Routes, rep.Dropped)
	}
}

func clientHost() File {
	cur := laptop()
	cur.Repos["client-a"] = Repo{Config: "/x", Kind: KindClient}
	return cur
}

func TestInheritLeavingFlagsDroppedClientRule(t *testing.T) {
	cur := clientHost()
	cur.Routes = []Rule{{Folder: "-Users-zed-acme*", Repo: "client-a"}, {Remote: "github.com/c/*", Repo: "client-a"}}
	_, rep, err := Template{Version: 1, Repos: map[string]string{}}.Inherit(cur, "-Users-zed", false, []string{"-Users-zed-acme-notes"})
	if err != nil {
		t.Fatal(err)
	}
	j := strings.Join(rep.Leaving, "\n")
	if len(rep.Leaving) < 3 || !strings.Contains(j, "client-a -> denied") || !strings.Contains(j, "github.com/c/*") {
		t.Fatalf("leaving: %v", rep.Leaving)
	}
}

func TestInheritLeavingFlagsReorderShadowingClientRule(t *testing.T) {
	cur := clientHost()
	cur.Routes = []Rule{{Folder: "-Users-zed-acme*", Repo: "client-a"}, {Folder: "-Users-zed-*", Repo: "projects"}}
	// The template lists the broad rule first: it would shadow the client rule.
	tpl := Template{Version: 1, Repos: map[string]string{"projects": KindProject, "client-a": KindClient},
		Routes: []TemplateRule{{Folder: "{home}-*", Repo: "projects"}, {Folder: "{home}-acme*", Repo: "client-a"}}}
	g, rep, err := tpl.Inherit(cur, "-Users-zed", false, []string{"-Users-zed-acme-notes", "-Users-zed-other"})
	if err != nil {
		t.Fatal(err)
	}
	if g.Routes[0].Repo != "projects" {
		t.Fatalf("test needs the reorder: %+v", g.Routes)
	}
	j := strings.Join(rep.Leaving, "\n")
	if !strings.Contains(j, "client-a -> projects") || strings.Contains(j, "other") {
		t.Fatalf("leaving: %v", rep.Leaving)
	}
	// Same order: nothing leaves.
	tpl.Routes[0], tpl.Routes[1] = tpl.Routes[1], tpl.Routes[0]
	if _, rep, _ := tpl.Inherit(cur, "-Users-zed", false, []string{"-Users-zed-acme-notes"}); len(rep.Leaving) != 0 {
		t.Fatalf("false positive: %v", rep.Leaving)
	}
}

func TestInheritLeavingFlagsHomeFromOrchAndFromDeniedOnClientHost(t *testing.T) {
	cur := clientHost()
	cur.Repos["orch-a"] = Repo{Config: "/x", Kind: KindOrch}
	tpl := Template{Version: 1, Repos: map[string]string{"personal": KindPersonal}, Routes: []TemplateRule{{Home: true, Repo: "personal"}}}
	// Denied before, host keeps client notes.
	_, rep, _ := tpl.Inherit(cur, "-Users-zed", true, nil)
	if len(rep.Leaving) != 1 || !strings.Contains(rep.Leaving[0], "home folder") {
		t.Fatalf("%v", rep.Leaving)
	}
	cur.Routes = []Rule{{Folder: "-Users-zed", Repo: "orch-a"}}
	_, rep, _ = tpl.Inherit(cur, "-Users-zed", true, nil)
	if len(rep.Leaving) == 0 || !strings.Contains(strings.Join(rep.Leaving, ""), "orch-a -> personal") {
		t.Fatalf("%v", rep.Leaving)
	}
	// A host without client repos is not asked.
	_, rep, _ = tpl.Inherit(laptop(), "-Users-zed", true, nil)
	if len(rep.Leaving) != 0 {
		t.Fatalf("%v", rep.Leaving)
	}
}

func TestDiffRoutes(t *testing.T) {
	a := []Rule{{Folder: "-a", Repo: "x"}, {Folder: "-b", Repo: "x"}}
	if d := DiffRoutes(a, a); len(d) != 0 {
		t.Fatal(d)
	}
	d := strings.Join(DiffRoutes(a, []Rule{{Folder: "-a", Repo: "x"}, {Folder: "-c", Repo: "y"}}), "\n")
	if !strings.Contains(d, "- folder -b -> x") || !strings.Contains(d, "+ folder -c -> y") {
		t.Fatal(d)
	}
}

func regRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	for k, v := range map[string]string{"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_SYSTEM": os.DevNull,
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.com", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.com"} {
		t.Setenv(k, v)
	}
	d := t.TempDir()
	gitIn(t, d, "init", "-q", "-b", "main")
	return d
}

func TestPublishWritesTemplateAndCommitsOnlyIt(t *testing.T) {
	reg := regRepo(t)
	os.WriteFile(filepath.Join(reg, "other.txt"), []byte("x"), 0600)
	gitIn(t, reg, "add", "other.txt") // staged by someone else: must stay out of our commit
	f := hostFile()
	f.Registry = reg
	p := writeRoutes(t, f)
	out, err := PublishTemplate(p, "", "laptop-2", "-home-zed", "/home/zed")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(reg, "hosts", "laptop-2", "routes.json"); out != want {
		t.Fatalf("%s", out)
	}
	b, _ := os.ReadFile(out)
	if strings.Contains(string(b), "/home/zed") {
		t.Fatalf("path leaked: %s", b)
	}
	files, _ := exec.Command("git", "-C", reg, "show", "--name-only", "--format=", "HEAD").Output()
	if strings.TrimSpace(string(files)) != "hosts/laptop-2/routes.json" {
		t.Fatalf("commit holds %q", files)
	}
	// Publishing the same thing again makes no new commit.
	n1, _ := exec.Command("git", "-C", reg, "rev-list", "--count", "HEAD").Output()
	if _, err := PublishTemplate(p, "", "laptop-2", "-home-zed", "/home/zed"); err != nil {
		t.Fatal(err)
	}
	n2, _ := exec.Command("git", "-C", reg, "rev-list", "--count", "HEAD").Output()
	if string(n1) != string(n2) {
		t.Fatal("empty republish committed")
	}
	// And read back.
	tpl, err := ReadTemplate(reg, "laptop-2")
	if err != nil || len(tpl.Routes) != 4 {
		t.Fatalf("%v %+v", err, tpl)
	}
}

func TestPublishRefusesBadLabelAndMissingRegistry(t *testing.T) {
	reg := regRepo(t)
	f := hostFile()
	f.Registry = reg
	p := writeRoutes(t, f)
	if _, err := PublishTemplate(p, "", "../evil", "-h", "/h"); err == nil {
		t.Fatal("bad label accepted")
	}
	if _, err := os.Stat(filepath.Join(reg, "hosts")); err == nil {
		t.Fatal("wrote for a bad label")
	}
	f.Registry = ""
	p = writeRoutes(t, f)
	if _, err := PublishTemplate(p, "", "ok", "-h", "/h"); err == nil {
		t.Fatal("no registry accepted")
	}
	if _, err := PublishTemplate(p, t.TempDir(), "ok", "-h", "/h"); err == nil {
		t.Fatal("registry that is no git repo accepted")
	}
}
