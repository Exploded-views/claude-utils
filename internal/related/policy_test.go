package related

import (
	"strings"
	"testing"
)

func TestCheck_Edges(t *testing.T) {
	c := Checker{}
	cases := []struct {
		name     string
		from, to string
		mode     Mode
		refuse   string // substring of the refusal, "" = allowed
	}{
		{"project reads project", "mem-project-a", "mem-project-b", ModeRead, ""},
		{"project joins project", "mem-project-a", "mem-project-b", ModeJoin, ""},
		{"project reads infra", "mem-project-a", "mem-infra-x", ModeRead, ""},
		{"personal reads project", "mem-personal", "mem-project-a", ModeRead, ""},
		{"same client reads", "mem-client-a", "mem-client-a", ModeRead, "itself"},
		{"join across kinds", "mem-project-a", "mem-infra-x", ModeJoin, "across kinds"},
		{"project to own client", "mem-project-a", "mem-client-a", ModeRead, "client"},
		{"client to personal", "mem-client-a", "mem-personal", ModeRead, "client"},
		{"cross client read", "mem-client-a", "mem-client-b", ModeRead, "client"},
		{"cross client join", "mem-client-a", "mem-client-b", ModeJoin, "client"},
		{"non-mem from", "notes-a", "mem-project-b", ModeRead, "mem-"},
		{"non-mem to", "mem-project-a", "other-repo", ModeRead, "mem-"},
		{"registry is no endpoint", "mem-project-a", "mem-registry", ModeRead, "kind"},
		{"orch-all reads orch-x", "mem-orch-all", "mem-orch-x", ModeRead, ""},
		{"orch-all joins orch-x", "mem-orch-all", "mem-orch-x", ModeJoin, "read"},
		{"orch-all to client repo", "mem-orch-all", "mem-client-x", ModeRead, "client"},
		{"orch-all to project", "mem-orch-all", "mem-project-a", ModeRead, "orch"},
		{"orch-x to orch-all", "mem-orch-x", "mem-orch-all", ModeRead, "orch"},
		{"orch-x to orch-y", "mem-orch-x", "mem-orch-y", ModeRead, "orch"},
		{"orch-x to own client repo", "mem-orch-x", "mem-client-x", ModeRead, "orch"},
		{"personal reads orch-all", "mem-personal", "mem-orch-all", ModeRead, "orch"},
		{"bad mode", "mem-project-a", "mem-project-b", Mode("write"), "mode"},
		{"path in name", "mem-project-a", "mem-project-../x", ModeRead, "name"},
	}
	for _, tc := range cases {
		err := c.Check(tc.from, tc.to, tc.mode)
		switch {
		case tc.refuse == "" && err != nil:
			t.Errorf("%s: refused: %v", tc.name, err)
		case tc.refuse != "" && err == nil:
			t.Errorf("%s: allowed, want refusal", tc.name)
		case err != nil && !strings.Contains(strings.ToLower(err.Error()), tc.refuse):
			t.Errorf("%s: %q lacks %q", tc.name, err, tc.refuse)
		}
	}
}

func TestCheck_SameClientAllowedWithClientField(t *testing.T) {
	nodes := map[string]Node{
		"mem-client-a-web": {Name: "mem-client-a-web", Kind: "client", Client: "a"},
		"mem-client-a-api": {Name: "mem-client-a-api", Kind: "client", Client: "a"},
		"mem-client-b":     {Name: "mem-client-b", Kind: "client", Client: "other"},
	}
	c := Checker{Lookup: func(n string) (Node, bool) { v, ok := nodes[n]; return v, ok }}
	if err := c.Check("mem-client-a-web", "mem-client-a-api", ModeRead); err != nil {
		t.Fatal(err)
	}
	if err := c.Check("mem-client-a-web", "mem-client-a-api", ModeJoin); err != nil {
		t.Fatal(err)
	}
	if err := c.Check("mem-client-a-web", "mem-client-b", ModeRead); err == nil {
		t.Fatal("cross-client edge allowed")
	}
}

func TestCheck_KindMismatchWithName(t *testing.T) {
	c := Checker{Lookup: func(n string) (Node, bool) {
		return Node{Name: n, Kind: "project"}, n == "mem-client-a"
	}}
	if err := c.Check("mem-client-a", "mem-project-b", ModeRead); err == nil {
		t.Fatal("a client-named repo declared as a project must be refused")
	}
}

type denyVis struct{ bad string }

func (d denyVis) CheckPrivate(repo string) error {
	if repo == d.bad {
		return errPublic
	}
	return nil
}

func TestCheck_VisibilityHook(t *testing.T) {
	c := Checker{Visible: denyVis{bad: "mem-project-b"}}
	if err := c.Check("mem-project-a", "mem-project-b", ModeRead); err == nil {
		t.Fatal("a repo the visibility check rejects must be refused")
	}
}

func lookupOf(nodes map[string]Node) func(string) (Node, bool) {
	return func(n string) (Node, bool) { v, ok := nodes[n]; return v, ok }
}

func TestCheck_DeclaredClientMustMatchTheName(t *testing.T) {
	for _, tc := range []struct {
		name   string
		node   Node
		wantOK bool
	}{
		{"mem-client-a", Node{Kind: "client", Client: "a"}, true},
		{"mem-client-a-web", Node{Kind: "client", Client: "a"}, true},
		{"mem-client-a-web", Node{Kind: "client", Client: "a-web"}, true},
		{"mem-client-a", Node{Kind: "client", Client: "b"}, false},     // claims another client
		{"mem-client-ab", Node{Kind: "client", Client: "a"}, false},    // prefix without the dash
		{"mem-client-a", Node{Kind: "client", Client: "a-web"}, false}, // longer than the name
		{"mem-project-a", Node{Kind: "project", Client: "a"}, false},   // client field on a project
		{"mem-orch-a", Node{Kind: "orch", Client: "a"}, true},
		{"mem-personal", Node{Kind: "personal", Client: "a"}, false},
	} {
		tc.node.Name = tc.name
		c := Checker{Lookup: lookupOf(map[string]Node{tc.name: tc.node})}
		_, err := c.node(tc.name)
		if (err == nil) != tc.wantOK {
			t.Errorf("%s declared %+v: err=%v, want ok=%v", tc.name, tc.node, err, tc.wantOK)
		}
	}
	// A false client claim cannot open a cross-client edge.
	c := Checker{Lookup: lookupOf(map[string]Node{"mem-client-b": {Name: "mem-client-b", Kind: "client", Client: "a"}})}
	if err := c.Check("mem-client-a", "mem-client-b", ModeRead); err == nil {
		t.Fatal("a client field made two clients one")
	}
}

func TestSameClient(t *testing.T) {
	if sameClient(Node{}, Node{}) {
		t.Fatal("two blank clients are not the same client")
	}
	if sameClient(Node{Client: "a"}, Node{Client: "b"}) || !sameClient(Node{Client: "a"}, Node{Client: "a"}) {
		t.Fatal("sameClient wrong")
	}
	// Through Check: a client repo with a blank name-derived client.
	c := Checker{}
	if err := c.Check("mem-client-", "mem-client-x", ModeRead); err == nil {
		t.Fatal("blank-client repo related to a client repo")
	}
}

func TestCheck_KindlessEnds(t *testing.T) {
	// Each end alone without a kind refuses (both directions), but a kind the
	// routes file supplies for a kind-less name is used.
	c := Checker{}
	if c.Check("mem-registry", "mem-project-a", ModeRead) == nil || c.Check("mem-project-a", "mem-registry", ModeRead) == nil {
		t.Fatal("kind-less end accepted")
	}
	c = Checker{Lookup: lookupOf(map[string]Node{"mem-registry": {Name: "mem-registry", Kind: "project"}})}
	if err := c.Check("mem-registry", "mem-project-a", ModeJoin); err != nil {
		t.Fatalf("declared kind ignored: %v", err)
	}
}

func TestCheck_ClientVersusOtherKindBothDirections(t *testing.T) {
	c := Checker{}
	for _, e := range [][2]string{{"mem-client-a", "mem-project-a"}, {"mem-project-a", "mem-client-a"}, {"mem-client-a", "mem-infra-e"}, {"mem-infra-e", "mem-client-a"}} {
		err := c.Check(e[0], e[1], ModeRead)
		if err == nil || !strings.Contains(err.Error(), "never to a") {
			t.Errorf("%v: %v", e, err)
		}
	}
}

func TestCheck_OrchAllEveryOtherTarget(t *testing.T) {
	c := Checker{}
	for _, to := range []string{"mem-personal", "mem-project-a", "mem-infra-e", "mem-registry"} {
		if err := c.Check(OrchAll, to, ModeRead); err == nil || !strings.Contains(err.Error(), "only relate to a mem-orch") {
			t.Errorf("orch-all -> %s: %v", to, err)
		}
	}
	if err := c.Check(OrchAll, "mem-client-a", ModeRead); err == nil || !strings.Contains(err.Error(), "client repo") {
		t.Errorf("orch-all -> client: %v", err)
	}
	if err := c.Check(OrchAll, "mem-orch-a", ModeJoin); err == nil || !strings.Contains(err.Error(), "must be read") {
		t.Errorf("orch-all join: %v", err)
	}
}

type recVis struct{ asked *[]string }

func (r recVis) CheckPrivate(repo string) error { *r.asked = append(*r.asked, repo); return nil }

// Check validates the mode first (only the exact values read and join), and
// asks the visibility hook about both ends of an allowed relation; the zero
// Checker falls back to the name check.
func TestCheck_ModeIsValidatedBeforeVisibilityAndBothEndsAreAsked(t *testing.T) {
	for _, m := range []Mode{"", "READ", "Join", "read ", "write", "readjoin"} {
		var asked []string
		c := Checker{Visible: recVis{&asked}}
		if err := c.Check("mem-project-a", "mem-project-b", m); err == nil || !strings.Contains(err.Error(), "mode") {
			t.Errorf("mode %q: err = %v, want a mode refusal", m, err)
		}
		if len(asked) != 0 {
			t.Errorf("mode %q: visibility asked before the mode was refused: %v", m, asked)
		}
	}
	for _, m := range []Mode{ModeRead, ModeJoin} {
		var asked []string
		c := Checker{Visible: recVis{&asked}}
		if err := c.Check("mem-project-a", "mem-project-b", m); err != nil {
			t.Fatalf("mode %q: %v", m, err)
		}
		if len(asked) != 2 || asked[0] != "mem-project-a" || asked[1] != "mem-project-b" {
			t.Errorf("mode %q: visibility asked %v, want both ends in order", m, asked)
		}
	}
	// The ghguard-backed hook may refuse either end.
	for _, bad := range []string{"mem-project-a", "mem-project-b"} {
		c := Checker{Visible: denyVis{bad: bad}}
		if err := c.Check("mem-project-a", "mem-project-b", ModeRead); err == nil || !strings.Contains(err.Error(), bad) {
			t.Errorf("rejecting %s: %v", bad, err)
		}
	}
	if err := (Checker{}).Check("mem-project-a", "notes", ModeRead); err == nil {
		t.Error("the zero Checker must still apply the name check")
	}
}
