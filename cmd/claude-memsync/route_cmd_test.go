package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/route"
)

// homeHost builds a fake HOME with a routes.json whose repos are the given
// name->kind (each with an existing config.json) and the given routes.
func homeHost(t *testing.T, kinds map[string]string, routes []map[string]string) (routesPath, homeFolder string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	repos := map[string]any{}
	personal := ""
	for n, k := range kinds {
		cp := filepath.Join(home, ".claudesync.d", n, "config.json")
		os.MkdirAll(filepath.Dir(cp), 0700)
		os.WriteFile(cp, []byte("{}"), 0600)
		repos[n] = map[string]any{"config": cp, "kind": k}
		if k == "personal" {
			personal = n
		}
	}
	rj := map[string]any{"repos": repos, "routes": routes, "default": "deny"}
	if personal != "" {
		rj["personal"] = personal
	}
	b, _ := json.Marshal(rj)
	routesPath = filepath.Join(home, ".config", "claude-memsync", "routes.json")
	os.MkdirAll(filepath.Dir(routesPath), 0700)
	os.WriteFile(routesPath, b, 0600)
	t.Setenv(routesEnv, routesPath)
	hf, _ := route.HomeFolder()
	return routesPath, hf
}

func runRouteCmd(t *testing.T, stdin string, tty bool, args ...string) (int, string, string) {
	t.Helper()
	old := stdinIsTTY
	stdinIsTTY = func() bool { return tty }
	defer func() { stdinIsTTY = old }()
	var out, errb bytes.Buffer
	code := routeCmd(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func routesOf(t *testing.T, p string) route.File {
	t.Helper()
	f, err := route.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

var stdKinds = map[string]string{"personal": "personal", "projects": "project", "lab": "infra", "orch-a": "orch", "client-a": "client"}

func TestRouteHome_SetReplaceDenyAndShow(t *testing.T) {
	p, hf := homeHost(t, stdKinds, []map[string]string{{"folder": "-x", "repo": "projects"}})
	if code, _, e := runRouteCmd(t, "", false, "home", "lab"); code != 0 {
		t.Fatalf("set: %d %s", code, e)
	}
	f := routesOf(t, p)
	if f.Routes[0].Folder != hf || f.Routes[0].Repo != "lab" || len(f.Routes) != 2 {
		t.Fatalf("%+v", f.Routes)
	}
	if code, _, _ := runRouteCmd(t, "", false, "home", "personal"); code != 0 {
		t.Fatal("replace failed")
	}
	f = routesOf(t, p)
	if len(f.Routes) != 2 || f.Routes[0].Repo != "personal" {
		t.Fatalf("pin not replaced: %+v", f.Routes)
	}
	code, out, _ := runRouteCmd(t, "", false, "home")
	if code != 0 || !strings.Contains(out, "personal") {
		t.Fatalf("show: %d %q", code, out)
	}
	if code, _, _ := runRouteCmd(t, "", false, "home", "deny"); code != 0 {
		t.Fatal("deny failed")
	}
	f = routesOf(t, p)
	if len(f.Routes) != 1 || f.Routes[0].Folder != "-x" {
		t.Fatalf("pin not removed: %+v", f.Routes)
	}
	_, out, _ = runRouteCmd(t, "", false, "home")
	if !strings.Contains(out, "denied") {
		t.Fatalf("show after deny: %q", out)
	}
}

func TestRouteHome_FolderOverrideAndUnknownRepo(t *testing.T) {
	p, _ := homeHost(t, stdKinds, nil)
	if code, _, e := runRouteCmd(t, "", false, "home", "--folder", "-other-home", "projects"); code != 0 {
		t.Fatalf("%s", e)
	}
	if f := routesOf(t, p); f.Routes[0].Folder != "-other-home" {
		t.Fatalf("%+v", f.Routes)
	}
	before, _ := os.ReadFile(p)
	if code, _, e := runRouteCmd(t, "", false, "home", "nope"); code == 0 || !strings.Contains(e, "nope") {
		t.Fatalf("unknown repo: %d %s", code, e)
	}
	if code, _, _ := runRouteCmd(t, "", false, "home", "--folder", "-a-*", "projects"); code == 0 {
		t.Fatal("glob folder accepted")
	}
	after, _ := os.ReadFile(p)
	if !bytes.Equal(before, after) {
		t.Fatal("a refused command changed the file")
	}
}

func TestRouteHome_ClientGuard(t *testing.T) {
	p, hf := homeHost(t, stdKinds, nil)
	if code, _, e := runRouteCmd(t, "", false, "home", "orch-a"); code != 0 {
		t.Fatalf("into orch: %s", e)
	}
	code, _, e := runRouteCmd(t, "", false, "home", "personal")
	if code == 0 || !strings.Contains(e, "--confirm-move-out-of-client") {
		t.Fatalf("out of orch without consent: %d %s", code, e)
	}
	if routesOf(t, p).Routes[0].Repo != "orch-a" {
		t.Fatal("refused move changed the file")
	}
	code, _, e = runRouteCmd(t, "", false, "home", "--confirm-move-out-of-client", "personal")
	if code != 0 {
		t.Fatalf("with consent: %s", e)
	}
	if !strings.Contains(e, "moved from client repo") || strings.Contains(e, hf) {
		t.Fatalf("move not logged by hash: %q", e)
	}
	// Client to client and client to deny need no consent.
	runRouteCmd(t, "", false, "home", "client-a")
	if code, _, _ := runRouteCmd(t, "", false, "home", "orch-a"); code != 0 {
		t.Fatal("client to orch refused")
	}
	if code, _, _ := runRouteCmd(t, "", false, "home", "deny"); code != 0 {
		t.Fatal("client to deny refused")
	}
}

func TestRouteHome_Suggest(t *testing.T) {
	p, _ := homeHost(t, stdKinds, nil)
	old := hostOS
	defer func() { hostOS = old }()
	hostOS = "darwin"
	_, out, _ := runRouteCmd(t, "", false, "home", "--suggest")
	if !strings.Contains(out, "suggest: personal") || !strings.Contains(out, "reason:") {
		t.Fatalf("%q", out)
	}
	hostOS = "linux"
	_, out, _ = runRouteCmd(t, "", false, "home", "--suggest")
	if !strings.Contains(out, "suggest: orch-a") {
		t.Fatalf("%q", out)
	}
	if len(routesOf(t, p).Routes) != 0 {
		t.Fatal("--suggest applied something")
	}
	p2, _ := homeHost(t, map[string]string{"personal": "personal"}, nil)
	_ = p2
	_, out, _ = runRouteCmd(t, "", false, "home", "--suggest")
	if !strings.Contains(out, "suggest: deny") {
		t.Fatalf("%q", out)
	}
}

func TestRouteHome_AskNeedsTTY(t *testing.T) {
	p, _ := homeHost(t, stdKinds, nil)
	code, _, e := runRouteCmd(t, "1\n", false, "home", "--ask")
	if code == 0 || !strings.Contains(e, "claude-memsync route home <repo>") {
		t.Fatalf("%d %s", code, e)
	}
	if len(routesOf(t, p).Routes) != 0 {
		t.Fatal("non-tty --ask changed the file")
	}
}

func TestRouteHome_AskChoices(t *testing.T) {
	old := hostOS
	defer func() { hostOS = old }()
	hostOS = "linux"
	p, hf := homeHost(t, stdKinds, nil)
	// Enter takes the suggestion (orch-a).
	code, out, e := runRouteCmd(t, "\n", true, "home", "--ask")
	if code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	for _, want := range []string{"personal", "project", "infra", "orch", "client", "deny", "(suggested)"} {
		if !strings.Contains(out, want) {
			t.Errorf("menu lacks %q:\n%s", want, out)
		}
	}
	if f := routesOf(t, p); f.Routes[0].Folder != hf || f.Routes[0].Repo != "orch-a" {
		t.Fatalf("%+v", f.Routes)
	}
	// By name, with consent needed to leave the orch repo.
	if code, _, e := runRouteCmd(t, "personal\n", true, "home", "--ask"); code == 0 || !strings.Contains(e, "--confirm-move-out-of-client") {
		t.Fatalf("%d %s", code, e)
	}
	if code, _, e := runRouteCmd(t, "personal\n", true, "home", "--ask", "--confirm-move-out-of-client"); code != 0 {
		t.Fatalf("%s", e)
	}
	if routesOf(t, p).Routes[0].Repo != "personal" {
		t.Fatal("named choice not applied")
	}
	// By number: deny is last.
	_, menu, _ := runRouteCmd(t, "x\n", true, "home", "--ask")
	var denyN string
	for _, l := range strings.Split(menu, "\n") {
		if i := strings.Index(l, ") deny"); i > 0 {
			denyN = strings.TrimSpace(l[:i])
		}
	}
	if denyN == "" {
		t.Fatalf("no numbered deny in menu:\n%s", menu)
	}
	if code, _, e := runRouteCmd(t, denyN+"\n", true, "home", "--ask"); code != 0 {
		t.Fatalf("%s", e)
	}
	if len(routesOf(t, p).Routes) != 0 {
		t.Fatalf("deny by number: %+v", routesOf(t, p).Routes)
	}
	// Junk is refused.
	if code, _, _ := runRouteCmd(t, "zzz\n", true, "home", "--ask"); code == 0 {
		t.Fatal("junk choice accepted")
	}
}

func TestRoutePublishInherit_EndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	for k, v := range map[string]string{"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_SYSTEM": os.DevNull,
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.com", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.com"} {
		t.Setenv(k, v)
	}
	// Host A: has everything, a registry repo, an orch home pin.
	pa, hfA := homeHost(t, stdKinds, []map[string]string{
		{"folder": "placeholder", "repo": "projects"},
		{"remote": "github.com/acme/**", "repo": "client-a"},
		{"remote": "github.com/zed/*", "repo": "projects"},
	})
	reg := t.TempDir()
	if out, err := exec.Command("git", "-C", reg, "init", "-q", "-b", "main").CombinedOutput(); err != nil {
		t.Fatalf("%s", out)
	}
	fa := routesOf(t, pa)
	fa.Registry = reg
	b, _ := json.Marshal(fa)
	os.WriteFile(pa, b, 0600)
	runRouteCmd(t, "", false, "home", "orch-a")
	if code, _, e := runRouteCmd(t, "", false, "publish", "--as", "Bad Label"); code == 0 {
		t.Fatalf("bad label accepted: %s", e)
	}
	if code, out, e := runRouteCmd(t, "", false, "publish", "--as", "laptop-2"); code != 0 || !strings.Contains(out, "laptop-2") {
		t.Fatalf("publish: %d %s %s", code, out, e)
	}
	tb, _ := os.ReadFile(filepath.Join(reg, "hosts", "laptop-2", "routes.json"))
	if strings.Contains(string(tb), hfA) || strings.Contains(string(tb), "config") {
		t.Fatalf("template leaks: %s", tb)
	}

	// Host B: a laptop with only personal + projects.
	pb, hfB := homeHost(t, map[string]string{"personal": "personal", "projects": "project"}, nil)
	fb := routesOf(t, pb)
	fb.Registry = reg
	b, _ = json.Marshal(fb)
	os.WriteFile(pb, b, 0600)

	// Without --yes and without a TTY: shows the diff, refuses.
	code, out, e := runRouteCmd(t, "", false, "inherit", "laptop-2")
	if code == 0 || !strings.Contains(e, "--yes") || !strings.Contains(out, "+ remote github.com/zed/* -> projects") {
		t.Fatalf("%d out=%s err=%s", code, out, e)
	}
	if !strings.Contains(out, "dropped") || !strings.Contains(out, "client-a") {
		t.Fatalf("no drop report: %s", out)
	}
	if len(routesOf(t, pb).Routes) != 0 {
		t.Fatal("applied without consent")
	}
	// With --yes, without --home: the template's home pin is not applied.
	if code, _, e := runRouteCmd(t, "", false, "inherit", "laptop-2", "--yes"); code != 0 {
		t.Fatalf("%s", e)
	}
	g := routesOf(t, pb)
	for _, r := range g.Routes {
		if r.Repo == "client-a" || r.Repo == "orch-a" {
			t.Fatalf("added a route to a repo this host lacks: %+v", r)
		}
	}
	if _, ok := g.HomePin(hfB); ok {
		t.Fatal("home pin applied without --home")
	}
	// Flag after the label also works, and a template whose home repo this
	// host lacks drops the pin even with --home.
	if code, _, e := runRouteCmd(t, "", false, "inherit", "--home", "--yes", "laptop-2"); code != 0 {
		t.Fatalf("%s", e)
	}
	if _, ok := routesOf(t, pb).HomePin(hfB); ok {
		t.Fatal("pinned the home folder to a repo this host lacks")
	}
	if code, _, _ := runRouteCmd(t, "", false, "inherit", "no-such", "--yes"); code == 0 {
		t.Fatal("missing template accepted")
	}
	if code, _, _ := runRouteCmd(t, "", false, "inherit", "BAD", "--yes"); code == 0 {
		t.Fatal("bad label accepted")
	}
}

func TestRouteInherit_HomeAppliesWithFlagAndConfirmPrompt(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	_, hf := homeHost(t, map[string]string{"personal": "personal", "projects": "project"}, nil)
	reg := t.TempDir()
	os.MkdirAll(filepath.Join(reg, "hosts", "mac"), 0700)
	tpl := `{"version":1,"repos":{"personal":"personal","projects":"project"},"routes":[{"home":true,"repo":"personal"}]}`
	os.WriteFile(filepath.Join(reg, "hosts", "mac", "routes.json"), []byte(tpl), 0600)
	// Interactive: "n" declines, "y" applies.
	if code, _, _ := runRouteCmd(t, "n\n", true, "inherit", "mac", "--home", "--registry", reg); code == 0 {
		t.Fatal("declined prompt reported success")
	}
	p := os.Getenv(routesEnv)
	if len(routesOf(t, p).Routes) != 0 {
		t.Fatal("applied after n")
	}
	if code, _, e := runRouteCmd(t, "y\n", true, "inherit", "mac", "--home", "--registry", reg); code != 0 {
		t.Fatalf("%s", e)
	}
	if r, ok := routesOf(t, p).HomePin(hf); !ok || r != "personal" {
		t.Fatalf("home pin: %q %v", r, ok)
	}
}

func TestRouteInherit_LosingClientRoutesNeedsConsent(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	p, _ := homeHost(t, map[string]string{"personal": "personal", "client-a": "client"},
		[]map[string]string{{"remote": "github.com/c/*", "repo": "client-a"}})
	reg := t.TempDir()
	os.MkdirAll(filepath.Join(reg, "hosts", "bare"), 0700)
	os.WriteFile(filepath.Join(reg, "hosts", "bare", "routes.json"), []byte(`{"version":1,"repos":{},"routes":[]}`), 0600)
	code, _, e := runRouteCmd(t, "", false, "inherit", "bare", "--yes", "--registry", reg)
	if code == 0 || !strings.Contains(e, "--confirm-move-out-of-client") {
		t.Fatalf("%d %s", code, e)
	}
	if len(routesOf(t, p).Routes) != 1 {
		t.Fatal("client rule removed without consent")
	}
	if code, _, e := runRouteCmd(t, "", false, "inherit", "bare", "--yes", "--registry", reg, "--confirm-move-out-of-client"); code != 0 {
		t.Fatalf("%s", e)
	}
	if len(routesOf(t, p).Routes) != 0 {
		t.Fatal("not applied with consent")
	}
}
