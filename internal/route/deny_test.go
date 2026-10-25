package route

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDenyRuleDeniesAndDoesNotFallThrough(t *testing.T) {
	f := fileOf(Rule{Folder: "-home-x-acme*", Deny: true}, Rule{Folder: "-home-x-*", Repo: "projects"})
	if err := f.validate(); err != nil {
		t.Fatal(err)
	}
	r := testRouter(t, f, []string{"personal", "projects"}, nil)
	if d := r.Decide("-home-x-acme-notes"); d.Repo != "" || d.Reason != ReasonDenied || d.Rule != 0 {
		t.Fatalf("%+v", d)
	}
	if d := r.Decide("-home-x-other"); d.Repo != "projects" {
		t.Fatalf("%+v", d)
	}
}

func TestDenyRuleValidation(t *testing.T) {
	for name, r := range map[string]Rule{
		"deny with a repo":   {Folder: "-a", Deny: true, Repo: "projects"},
		"no repo, no deny":   {Folder: "-a"},
		"deny with no match": {Deny: true},
		"deny, two matchers": {Folder: "-a", Path: "~/x", Deny: true},
	} {
		if err := fileOf(r).validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for name, r := range map[string]Rule{
		"folder": {Folder: "-a*", Deny: true},
		"path":   {Path: "~/x/**", Deny: true},
		"remote": {Remote: "github.com/c/*", Aliases: []string{"github.com/d/*"}, Deny: true},
	} {
		if err := fileOf(r).validate(); err != nil {
			t.Errorf("%s refused: %v", name, err)
		}
	}
}

func TestReservedRepoNamesRefused(t *testing.T) {
	for _, n := range []string{"a:b", "unavailable", "unavailable-x", "deny", "denylist"} {
		f := fileOf()
		f.Repos[n] = Repo{Config: "/x", Kind: KindProject}
		if err := f.validate(); err == nil {
			t.Errorf("repo name %q accepted", n)
		}
	}
}

func clientHostFile() File {
	f := hostFile()
	f.Routes = []Rule{
		{Folder: "-home-zed", Repo: "orch-a"},
		{Folder: "-home-zed-acme*", Repo: "client-a"},
		{Deny: true, Path: "/home/zed/secret/**"},
		{Folder: "-home-zed-*", Repo: "projects"},
	}
	f.Repos["client-a"] = Repo{Config: "/x/c", Kind: KindClient}
	return f
}

func TestPublishedClientRulesBecomeDenyEntries(t *testing.T) {
	tpl := clientHostFile().Template("-home-zed", "/home/zed")
	b, _ := json.Marshal(tpl)
	s := string(b)
	for _, leak := range []string{"client-a", "orch-a"} {
		if strings.Contains(s, leak) {
			t.Errorf("template names %q: %s", leak, s)
		}
	}
	if len(tpl.Routes) != 4 || !tpl.Routes[1].Deny || tpl.Routes[1].Folder != "{home}-acme*" || tpl.Routes[1].Repo != "" || !tpl.Routes[2].Deny {
		t.Fatalf("%+v", tpl.Routes)
	}
	if !tpl.Routes[0].Deny || tpl.Routes[0].Home {
		t.Fatalf("home pin to an orch repo must not publish as a home pin: %+v", tpl.Routes[0])
	}
	// The Mac inherits: client-named folders are denied, not caught by the
	// broad rule behind them.
	mac := laptop()
	g, _, err := tpl.Inherit(mac, "-Users-zed", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := testRouter(t, g, []string{"personal", "projects"}, nil)
	r.Candidates = func(string) []string { return []string{"/nonexistent/x"} }
	if d := r.Decide("-Users-zed-acme-notes"); d.Repo != "" {
		t.Fatalf("client folder routed on the Mac: %+v", d)
	}
	if d := r.Decide("-Users-zed-other"); d.Repo != "projects" {
		t.Fatalf("%+v", d)
	}
}

func TestInheritKeepsDenyEntriesInOrder(t *testing.T) {
	tpl := Template{Version: 1, Repos: map[string]string{"projects": KindProject}, Routes: []TemplateRule{
		{Folder: "{home}-a*", Deny: true}, {Path: "{home}/b/**", Deny: true}, {Folder: "{home}-*", Repo: "projects"}}}
	g, rep, err := tpl.Inherit(laptop(), "-Users-zed", false, nil)
	if err != nil || len(rep.Dropped) != 0 || len(g.Routes) != 3 {
		t.Fatalf("%v %v %+v", err, rep, g.Routes)
	}
	if !g.Routes[0].Deny || g.Routes[0].Folder != "-Users-zed-a*" || !g.Routes[1].Deny || g.Routes[1].Path != "~/b/**" || g.Routes[2].Repo != "projects" {
		t.Fatalf("%+v", g.Routes)
	}
}

// Shadowing across rule kinds (P4-P6): the client rule stays in the table but
// a non-client rule of another kind is now ahead of it.
func TestLeavingFlagsShadowingAcrossKinds(t *testing.T) {
	cases := map[string]struct {
		cur []Rule
		tpl []TemplateRule
	}{
		"P4 remote client rule shadowed by a path rule": {
			[]Rule{{Remote: "github.com/c/*", Repo: "client-a"}},
			[]TemplateRule{{Path: "~/**", Repo: "projects"}, {Remote: "github.com/c/*", Repo: "client-a"}}},
		"P5 path client rule shadowed by a remote rule": {
			[]Rule{{Path: "~/c/**", Repo: "client-a"}},
			[]TemplateRule{{Remote: "github.com/**", Repo: "projects"}, {Path: "~/c/**", Repo: "client-a"}}},
		"P6 folder client rule shadowed by a path rule": {
			[]Rule{{Folder: "-Users-zed-c*", Repo: "client-a"}},
			[]TemplateRule{{Path: "~/**", Repo: "projects"}, {Folder: "-Users-zed-c*", Repo: "client-a"}}},
		"deny rule shadowed": {
			[]Rule{{Deny: true, Folder: "-Users-zed-c*"}},
			[]TemplateRule{{Remote: "github.com/**", Repo: "projects"}, {Deny: true, Folder: "-Users-zed-c*"}}},
		"client rule changed": {
			[]Rule{{Folder: "-Users-zed-c*", Repo: "client-a"}},
			[]TemplateRule{{Folder: "-Users-zed-c", Repo: "client-a"}}},
	}
	for name, c := range cases {
		cur := clientHost()
		cur.Routes = c.cur
		tpl := Template{Version: 1, Repos: map[string]string{"projects": KindProject, "client-a": KindClient}, Routes: c.tpl}
		g, rep, err := tpl.Inherit(cur, "-Users-zed", false, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(rep.Leaving) == 0 {
			t.Errorf("%s: not flagged; routes %+v", name, g.Routes)
		}
	}
	// Same rules, nothing added ahead: not flagged. A host without protected
	// rules is not asked.
	cur := clientHost()
	cur.Routes = []Rule{{Remote: "github.com/c/*", Repo: "client-a"}, {Path: "~/**", Repo: "projects"}}
	tpl := Template{Version: 1, Repos: map[string]string{"projects": KindProject, "client-a": KindClient},
		Routes: []TemplateRule{{Remote: "github.com/c/*", Repo: "client-a"}, {Path: "~/**", Repo: "projects"}, {Folder: "-x", Repo: "projects"}}}
	if _, rep, _ := tpl.Inherit(cur, "-Users-zed", false, nil); len(rep.Leaving) != 0 {
		t.Errorf("false positive: %v", rep.Leaving)
	}
	if _, rep, _ := tpl.Inherit(laptop(), "-Users-zed", false, nil); len(rep.Leaving) != 0 {
		t.Errorf("asked on a host without client rules: %v", rep.Leaving)
	}
}

func TestLeavingFlagsClientToOtherClientRepo(t *testing.T) {
	cur := clientHost()
	cur.Repos["client-b"] = Repo{Config: "/x/b", Kind: KindClient}
	cur.Routes = []Rule{{Folder: "-Users-zed-acme*", Repo: "client-a"}}
	tpl := Template{Version: 1, Repos: map[string]string{"client-b": KindClient}, Routes: []TemplateRule{{Folder: "{home}-acme*", Repo: "client-b"}}}
	_, rep, err := tpl.Inherit(cur, "-Users-zed", false, []string{"-Users-zed-acme-x"})
	if err != nil || !strings.Contains(strings.Join(rep.Leaving, "\n"), "client-a -> client-b") {
		t.Fatalf("%v %v", err, rep.Leaving)
	}
}
