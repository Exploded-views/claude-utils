package related

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

func gitEnv(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
}

func newRegistryRepo(t *testing.T) string {
	t.Helper()
	gitEnv(t)
	dir := t.TempDir()
	if err := gitwt.New(dir).Init("main"); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRegistry_ParseEditCommit(t *testing.T) {
	dir := newRegistryRepo(t)
	reg, err := LoadRegistry(dir)
	if err != nil || len(reg.Entries) != 0 {
		t.Fatalf("missing file must load empty: %v %v", reg, err)
	}
	c := Checker{}
	if err := reg.Relate(c, "mem-project-a", "mem-project-b", ModeRead); err != nil {
		t.Fatal(err)
	}
	if err := reg.Relate(c, "mem-project-a", "mem-infra-x", ModeRead); err != nil {
		t.Fatal(err)
	}
	// Changing the mode edits in place.
	if err := reg.Relate(c, "mem-project-a", "mem-project-b", ModeJoin); err != nil {
		t.Fatal(err)
	}
	if err := reg.Save("relate: test"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(reg.Path())
	for _, want := range []string{`"repo": "mem-project-a"`, `"mode": "join"`, `"mode": "read"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("file lacks %s:\n%s", want, b)
		}
	}
	out, err := gitwt.New(dir).Run("log", "--format=%s")
	if err != nil || strings.TrimSpace(out) != "relate: test" {
		t.Fatalf("registry commit: %q %v", out, err)
	}
	again, err := LoadRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	edges := again.Edges()
	if len(edges) != 2 || edges[0].To != "mem-infra-x" || edges[1].Mode != ModeJoin {
		t.Fatalf("edges = %+v", edges)
	}
	if !again.Unrelate("mem-project-a", "mem-infra-x") || again.Unrelate("mem-project-a", "mem-infra-x") {
		t.Fatal("Unrelate result wrong")
	}
	again.Unrelate("mem-project-a", "mem-project-b")
	if len(again.Entries) != 0 {
		t.Fatalf("empty entry kept: %+v", again.Entries)
	}
	if err := again.Save("unrelate: test"); err != nil {
		t.Fatal(err)
	}
}

func TestRegistry_RefusedEdgeIsNotWritten(t *testing.T) {
	dir := newRegistryRepo(t)
	reg, _ := LoadRegistry(dir)
	if err := reg.Relate(Checker{}, "mem-client-a", "mem-client-b", ModeRead); err == nil {
		t.Fatal("cross-client relation accepted")
	}
	if len(reg.Entries) != 0 {
		t.Fatal("a refused relation must leave the registry untouched")
	}
}

func TestRegistry_LoadTimeEnforcement(t *testing.T) {
	dir := newRegistryRepo(t)
	body := `[{"repo":"mem-client-a","related":[{"repo":"mem-client-b","mode":"read"}]},
	{"repo":"mem-orch-all","related":[{"repo":"mem-orch-a","mode":"read"},{"repo":"mem-orch-b","mode":"join"},{"repo":"mem-client-a","mode":"read"}]},
	{"repo":"mem-project-a","related":[{"repo":"plain-repo","mode":"read"}]}]`
	if err := os.WriteFile(dir+"/"+FileName, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	reg, err := LoadRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	ok, refused := reg.Active(Checker{})
	if len(ok) != 1 || ok[0].From != OrchAll || ok[0].To != "mem-orch-a" {
		t.Fatalf("active = %+v", ok)
	}
	if len(refused) != 4 {
		t.Fatalf("refused = %+v", refused)
	}
}

func TestRegistry_BadJSON(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(dir+"/"+FileName, []byte("{"), 0600)
	if _, err := LoadRegistry(dir); err == nil {
		t.Fatal("bad json accepted")
	}
}
