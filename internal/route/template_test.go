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
			{Folder: "-home-zed", Repo: "orch-a"},
			{Path: "~/clients/acme/**", Repo: "orch-a"},
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

func TestTemplateStripsPathsKeysAndHomeName(t *testing.T) {
	tpl := hostFile().Template("-home-zed")
	b, _ := json.Marshal(tpl)
	s := string(b)
	for _, leak := range []string{"/home/zed", "config", ".claudesync", "registry", "-home-zed", "://", "@"} {
		if strings.Contains(s, leak) {
			t.Errorf("template contains %q: %s", leak, s)
		}
	}
	if tpl.Repos["orch-a"] != KindOrch || tpl.Repos["lab"] != KindInfra || len(tpl.Repos) != 4 {
		t.Fatalf("repos: %v", tpl.Repos)
	}
	if !tpl.Routes[0].Home || tpl.Routes[0].Repo != "orch-a" || tpl.Routes[0].Folder != "" {
		t.Fatalf("home rule: %+v", tpl.Routes[0])
	}
	if tpl.Routes[2].Remote != "github.com/acme/**" || len(tpl.Routes[2].Aliases) != 1 {
		t.Fatalf("remote rule: %+v", tpl.Routes[2])
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

func TestInheritDropsAbsentReposAndNeverAddsClientRoute(t *testing.T) {
	tpl := hostFile().Template("-home-zed")
	g, rep, err := tpl.Inherit(laptop(), "-Users-zed", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range g.Routes {
		if r.Repo == "orch-a" || r.Repo == "lab" {
			t.Fatalf("added a route to a repo the host lacks: %+v", r)
		}
	}
	if len(g.Routes) != 1 || g.Routes[0].Remote != "github.com/zed/*" || g.Routes[0].Repo != "projects" {
		t.Fatalf("routes: %+v", g.Routes)
	}
	if len(rep.Dropped) != 3 {
		t.Fatalf("dropped: %v", rep.Dropped)
	}
	// Host settings stay the host's own.
	if g.Repos["projects"].Config != "/Users/zed/pr/config.json" || g.Registry != "/Users/zed/registry" || g.Personal != "personal" {
		t.Fatalf("host data changed: %+v", g)
	}
}

func TestInheritKindMismatchDropped(t *testing.T) {
	cur := laptop()
	cur.Repos["orch-a"] = Repo{Config: "/x", Kind: KindProject} // same name, other kind
	g, rep, err := hostFile().Template("-home-zed").Inherit(cur, "-Users-zed", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range g.Routes {
		if r.Repo == "orch-a" {
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
	tpl := hostFile().Template("-home-zed")
	tpl.Repos["personal"] = KindPersonal
	tpl.Routes[0].Repo = "lab"

	g, _, err := tpl.Inherit(cur, "-Users-zed", false)
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := g.HomePin("-Users-zed"); !ok || r != "personal" {
		t.Fatalf("own home pin not kept: %q %v", r, ok)
	}
	g, _, err = tpl.Inherit(cur, "-Users-zed", true)
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := g.HomePin("-Users-zed"); !ok || r != "lab" || g.Routes[0].Folder != "-Users-zed" {
		t.Fatalf("template home pin not applied on top: %q %+v", r, g.Routes)
	}
	if _, ok := g.HomePin("-home-zed"); ok {
		t.Fatal("the other host's home folder name leaked in")
	}
}

func TestInheritPersonalNeedsPin(t *testing.T) {
	tpl := Template{Version: 1, Repos: map[string]string{"personal": KindPersonal},
		Routes: []TemplateRule{{Remote: "github.com/zed/*", Repo: "personal"}, {Home: true, Repo: "personal"}}}
	g, rep, err := tpl.Inherit(laptop(), "-Users-zed", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Routes) != 1 || g.Routes[0].Folder != "-Users-zed" || len(rep.Dropped) != 1 {
		t.Fatalf("%+v %v", g.Routes, rep.Dropped)
	}
}

func TestInheritFlagsLostClientRoutes(t *testing.T) {
	cur := laptop()
	cur.Repos["client-a"] = Repo{Config: "/x", Kind: KindClient}
	cur.Routes = append(cur.Routes, Rule{Remote: "github.com/c/*", Repo: "client-a"})
	_, rep, err := hostFile().Template("-home-zed").Inherit(cur, "-Users-zed", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.LostClient) != 1 {
		t.Fatalf("lost client routes: %v", rep.LostClient)
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
	out, err := PublishTemplate(p, "", "laptop-2", "-home-zed")
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
	if _, err := PublishTemplate(p, "", "laptop-2", "-home-zed"); err != nil {
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
	if _, err := PublishTemplate(p, "", "../evil", "-h"); err == nil {
		t.Fatal("bad label accepted")
	}
	if _, err := os.Stat(filepath.Join(reg, "hosts")); err == nil {
		t.Fatal("wrote for a bad label")
	}
	f.Registry = ""
	p = writeRoutes(t, f)
	if _, err := PublishTemplate(p, "", "ok", "-h"); err == nil {
		t.Fatal("no registry accepted")
	}
	if _, err := PublishTemplate(p, t.TempDir(), "ok", "-h"); err == nil {
		t.Fatal("registry that is no git repo accepted")
	}
}

func TestInheritFlagsHomeLeavingClient(t *testing.T) {
	cur := laptop()
	cur.Repos["orch-a"] = Repo{Config: "/x", Kind: KindOrch}
	cur.Routes = []Rule{{Folder: "-Users-zed", Repo: "orch-a"}}
	tpl := Template{Version: 1, Repos: map[string]string{"personal": KindPersonal}, Routes: []TemplateRule{{Home: true, Repo: "personal"}}}
	_, rep, err := tpl.Inherit(cur, "-Users-zed", true)
	if err != nil || rep.HomeOut != "orch-a -> personal" {
		t.Fatalf("%v %+v", err, rep)
	}
}
