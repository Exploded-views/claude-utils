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
		"mem-client-a-web": {Name: "mem-client-a-web", Kind: "client", Client: "acme"},
		"mem-client-a-api": {Name: "mem-client-a-api", Kind: "client", Client: "acme"},
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
