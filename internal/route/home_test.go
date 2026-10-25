package route

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPersonalPinAccepted(t *testing.T) {
	f := fileOf(Rule{Folder: "-home-me", Repo: "personal"})
	if err := f.validate(); err != nil {
		t.Fatalf("exact folder pin to personal refused: %v", err)
	}
}

func TestPersonalRefusedFromGlobPathRemote(t *testing.T) {
	for name, r := range map[string]Rule{
		"folder glob star":     {Folder: "-home-me-*", Repo: "personal"},
		"folder glob question": {Folder: "-home-m?", Repo: "personal"},
		"folder double star":   {Folder: "**", Repo: "personal"},
		"path":                 {Path: "~/notes/**", Repo: "personal"},
		"remote":               {Remote: "github.com/me/*", Repo: "personal"},
	} {
		if err := fileOf(r).validate(); err == nil {
			t.Errorf("%s: rule to the personal repo accepted", name)
		}
	}
	// A repo of kind personal is covered even when "personal" does not name it.
	f := fileOf(Rule{Remote: "github.com/me/*", Repo: "personal"})
	f.Personal = ""
	if err := f.validate(); err == nil {
		t.Error("kind personal repo accepted as a remote target")
	}
}

func TestOwnsPersonalOnlyForPinnedFolder(t *testing.T) {
	f := fileOf(
		Rule{Folder: "-home-me", Repo: "personal"},
		Rule{Folder: "-home-me-*", Repo: "projects"},
	)
	r := testRouter(t, f, []string{"personal", "projects"}, nil)
	own := r.Owns("personal")
	if !own("-home-me") {
		t.Fatal("pinned folder not owned by personal")
	}
	if own("-home-me-app") || own("-other") {
		t.Fatal("personal owns more than the pinned folder")
	}
	if !r.Owns("projects")("-home-me-app") || r.Owns("projects")("-home-me") {
		t.Fatal("projects ownership wrong")
	}
	if !r.NonClient("-home-me") {
		t.Fatal("pinned personal folder must feed the catalog")
	}
	s := r.Summarize([]string{"-home-me", "-home-me-app", "-x"})
	if s.Owned["personal"] != 1 || s.Denied != 1 {
		t.Fatalf("%+v", s)
	}
}

// An unvalidated table (Router.New does not validate) must not let a glob own
// folders for the personal repo.
func TestRouterDeniesPersonalGlobEvenUnvalidated(t *testing.T) {
	f := fileOf(Rule{Folder: "-home-*", Repo: "personal"})
	r := testRouter(t, f, []string{"personal"}, nil)
	if r.Owns("personal")("-home-me") {
		t.Fatal("glob rule gave the personal repo a folder")
	}
}

func writeRoutes(t *testing.T, f File) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "routes.json")
	b, _ := json.Marshal(f)
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSetHomePinPutsPinOnTopAndReplacesOld(t *testing.T) {
	p := writeRoutes(t, fileOf(
		Rule{Folder: "-home-me-app", Repo: "projects"},
		Rule{Folder: "-home-me", Repo: "projects"},
	))
	ch, err := SetHomePin(p, "-home-me", "personal", HomeOpts{ConfirmMoveOutOfClient: true})
	if err != nil || !ch.Changed {
		t.Fatalf("%+v %v", ch, err)
	}
	f, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Routes) != 2 || f.Routes[0].Folder != "-home-me" || f.Routes[0].Repo != "personal" || f.Routes[1].Folder != "-home-me-app" {
		t.Fatalf("%+v", f.Routes)
	}
	// Setting it again changes nothing.
	if ch, err := SetHomePin(p, "-home-me", "personal", HomeOpts{ConfirmMoveOutOfClient: true}); err != nil || ch.Changed {
		t.Fatalf("second set: %+v %v", ch, err)
	}
}

func TestSetHomePinUnknownRepoRefused(t *testing.T) {
	p := writeRoutes(t, fileOf())
	if _, err := SetHomePin(p, "-home-me", "nope", HomeOpts{}); err == nil {
		t.Fatal("repo not in repos accepted")
	}
	if _, err := SetHomePin(p, "-home-*", "projects", HomeOpts{ConfirmMoveOutOfClient: true}); err == nil {
		t.Fatal("glob folder accepted")
	}
}

func TestSetHomePinDenyWritesDenyPinAndUnpinRemoves(t *testing.T) {
	p := writeRoutes(t, fileOf(
		Rule{Folder: "-home-me", Repo: "projects"},
		Rule{Folder: "-home-me-app", Repo: "projects"},
	))
	ch, err := SetHomePin(p, "-home-me", "", HomeOpts{})
	if err != nil || !ch.Changed {
		t.Fatal(ch, err)
	}
	f, _ := Load(p)
	if len(f.Routes) != 2 || f.Routes[0].Folder != "-home-me" || !f.Routes[0].Deny || f.Routes[0].Repo != "" {
		t.Fatalf("%+v", f.Routes)
	}
	if r, ok := f.HomePin("-home-me"); !ok || r != "" {
		t.Fatal("deny pin not seen as a pin")
	}
	if ch, err := SetHomePin(p, "-home-me", "", HomeOpts{}); err != nil || ch.Changed {
		t.Fatalf("second deny: %+v %v", ch, err)
	}
	if ch, err := SetHomePin(p, "-home-me", "", HomeOpts{Unpin: true}); err != nil || !ch.Changed {
		t.Fatal(ch, err)
	}
	f, _ = Load(p)
	if len(f.Routes) != 1 || f.Routes[0].Folder != "-home-me-app" {
		t.Fatalf("%+v", f.Routes)
	}
}

// N1: removing the pin would let the broader rule behind it take the home
// folder out of the client repo; deny keeps it denied.
func TestHomeDenyKeepsClientFolderDeniedUnpinNeedsConsent(t *testing.T) {
	f := fileOf(Rule{Folder: "-home-x", Repo: "client-a"}, Rule{Folder: "-home-x*", Repo: "projects"})
	p := writeRoutes(t, f)
	if _, err := SetHomePin(p, "-home-x", "", HomeOpts{Unpin: true}); !errors.Is(err, ErrNeedsConfirm) {
		t.Fatalf("unpin out of client: %v", err)
	}
	if g, _ := Load(p); len(g.Routes) != 2 || g.Routes[0].Repo != "client-a" {
		t.Fatal("refused unpin changed the file")
	}
	if _, err := SetHomePin(p, "-home-x", "", HomeOpts{}); err != nil {
		t.Fatal(err)
	}
	g, _ := Load(p)
	if got := g.Effective("-home-x"); got != "" {
		t.Fatalf("home folder routes to %q after deny", got)
	}
}

// N3: removing a deny pin on a host that keeps client notes.
func TestUnpinDenyPinOnClientHostNeedsConsent(t *testing.T) {
	f := fileOf(Rule{Folder: "-home-x", Deny: true}, Rule{Folder: "-home-x*", Repo: "projects"})
	p := writeRoutes(t, f)
	if _, err := SetHomePin(p, "-home-x", "", HomeOpts{Unpin: true}); !errors.Is(err, ErrNeedsConfirm) {
		t.Fatalf("%v", err)
	}
	if _, err := SetHomePin(p, "-home-x", "", HomeOpts{Unpin: true, ConfirmMoveOutOfClient: true}); err != nil {
		t.Fatal(err)
	}
	// Without client repos nothing is asked.
	g := f
	delete(g.Repos, "client-a")
	p = writeRoutes(t, g)
	if _, err := SetHomePin(p, "-home-x", "", HomeOpts{Unpin: true}); err != nil {
		t.Fatal(err)
	}
}

func TestSetHomePinClientGuard(t *testing.T) {
	for _, from := range []string{"client-a", "orch-a"} {
		f := fileOf(Rule{Folder: "-home-me", Repo: from})
		f.Repos["orch-a"] = Repo{Config: "/x/o/config.json", Kind: KindOrch}
		p := writeRoutes(t, f)
		var logs []string
		o := HomeOpts{Logf: func(format string, a ...any) { logs = append(logs, fmt.Sprintf(format, a...)) }}
		if _, err := SetHomePin(p, "-home-me", "personal", o); err == nil || !strings.Contains(err.Error(), "--confirm-move-out-of-client") {
			t.Fatalf("%s: client move out allowed without confirmation: %v", from, err)
		}
		if g, _ := Load(p); g.Routes[0].Repo != from {
			t.Fatal("refused move still wrote the file")
		}
		if len(logs) != 0 {
			t.Fatalf("logged a refused move: %v", logs)
		}
		o.ConfirmMoveOutOfClient = true
		if _, err := SetHomePin(p, "-home-me", "projects", o); err != nil {
			t.Fatal(err)
		}
		if len(logs) != 1 || !strings.Contains(logs[0], "moved from client repo") || strings.Contains(logs[0], "-home-me") {
			t.Fatalf("log: %v", logs)
		}
	}
}

func TestSetHomePinClientGuardCoversGlobRoutes(t *testing.T) {
	// The home folder reaches the client repo through a glob; the new pin on
	// top would take it out of there.
	p := writeRoutes(t, fileOf(Rule{Folder: "-home-*", Repo: "client-a"}))
	if _, err := SetHomePin(p, "-home-me", "projects", HomeOpts{}); err == nil {
		t.Fatal("move out of client through a glob allowed")
	}
	// Into a client repo, or to deny, needs no confirmation.
	if _, err := SetHomePin(p, "-home-me", "client-a", HomeOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := SetHomePin(p, "-home-me", "", HomeOpts{}); err != nil {
		t.Fatal(err)
	}
}

func TestSetHomePinLeavesBrokenFileAlone(t *testing.T) {
	p := filepath.Join(t.TempDir(), "routes.json")
	os.WriteFile(p, []byte("{nope"), 0600)
	if _, err := SetHomePin(p, "-h", "", HomeOpts{}); err == nil {
		t.Fatal("broken file accepted")
	}
	if b, _ := os.ReadFile(p); string(b) != "{nope" {
		t.Fatal("broken file overwritten")
	}
}

func TestHomeFolderEscapesHome(t *testing.T) {
	t.Setenv("HOME", "/home/a.b")
	t.Setenv("USERPROFILE", "/home/a.b")
	got, err := HomeFolder()
	if runtime.GOOS != "windows" && (err != nil || got != "-home-a-b") {
		t.Fatalf("%q %v", got, err)
	}
}

func TestSetHomePinNeedsConsentOnHostWithClientRepoEvenFromDenied(t *testing.T) {
	p := writeRoutes(t, fileOf()) // has client-a; the home folder is denied
	if _, err := SetHomePin(p, "-home-me", "projects", HomeOpts{}); !errors.Is(err, ErrNeedsConfirm) {
		t.Fatalf("got %v", err)
	}
	if _, err := SetHomePin(p, "-home-me", "projects", HomeOpts{ConfirmMoveOutOfClient: true}); err != nil {
		t.Fatal(err)
	}
	// A host without client repos needs none.
	f := fileOf()
	delete(f.Repos, "client-a")
	p = writeRoutes(t, f)
	if _, err := SetHomePin(p, "-home-me", "projects", HomeOpts{}); err != nil {
		t.Fatal(err)
	}
}

func TestEditKeepsUnknownKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "routes.json")
	in := `{"relatedRoot":"~/rel","relatedRemotes":["a/b"],"relatedOwners":["o"],"relatedFetchTimeoutSec":5,"relatedMaxMB":3,
"repos":{"projects":{"config":"/x/c.json","kind":"project","remote":"git@h:o/r.git","client":"acme"},
"client-a":{"config":"/x/d.json","kind":"client","extra":{"n":1}}},
"routes":[{"folder":"-a","repo":"projects"}],"default":"deny"}`
	os.WriteFile(p, []byte(in), 0600)
	if _, err := SetHomePin(p, "-home-me", "client-a", HomeOpts{}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	var got map[string]any
	json.Unmarshal(b, &got)
	for _, k := range []string{"relatedRoot", "relatedRemotes", "relatedOwners", "relatedFetchTimeoutSec", "relatedMaxMB"} {
		if _, ok := got[k]; !ok {
			t.Errorf("lost %s: %s", k, b)
		}
	}
	repos := got["repos"].(map[string]any)
	if repos["projects"].(map[string]any)["remote"] != "git@h:o/r.git" || repos["projects"].(map[string]any)["client"] != "acme" || repos["client-a"].(map[string]any)["extra"] == nil {
		t.Fatalf("repo keys lost: %s", b)
	}
	if len(got["routes"].([]any)) != 2 {
		t.Fatalf("%s", b)
	}
	// A rule with a field this version does not know is refused, untouched.
	bad := strings.Replace(in, `{"folder":"-a","repo":"projects"}`, `{"folder":"-a","repo":"projects","note":"x"}`, 1)
	os.WriteFile(p, []byte(bad), 0600)
	if _, err := SetHomePin(p, "-home-me", "client-a", HomeOpts{}); err == nil {
		t.Fatal("rule with unknown field rewritten")
	}
	if after, _ := os.ReadFile(p); string(after) != bad {
		t.Fatal("file changed")
	}
}

func TestFolderCaseFoldOnlyWhereRoutingFolds(t *testing.T) {
	f := fileOf(Rule{Folder: "-Home-Me", Repo: "projects"})
	_, ok := f.HomePin("-home-me")
	if ok != foldCase {
		t.Fatalf("pin lookup fold = %v, routing fold = %v", ok, foldCase)
	}
}

func TestSetHomePinClientToOtherClientNeedsConsent(t *testing.T) {
	f := fileOf(Rule{Folder: "-home-me", Repo: "client-a"})
	f.Repos["client-b"] = Repo{Config: "/x/b", Kind: KindClient}
	f.Repos["orch-b"] = Repo{Config: "/x/o", Kind: KindOrch}
	p := writeRoutes(t, f)
	for _, to := range []string{"client-b", "orch-b"} {
		if _, err := SetHomePin(p, "-home-me", to, HomeOpts{}); !errors.Is(err, ErrNeedsConfirm) {
			t.Fatalf("%s: %v", to, err)
		}
		if g, _ := Load(p); g.Routes[0].Repo != "client-a" {
			t.Fatal("refused move changed the file")
		}
	}
	var logs []string
	o := HomeOpts{ConfirmMoveOutOfClient: true, Logf: func(format string, a ...any) { logs = append(logs, fmt.Sprintf(format, a...)) }}
	if _, err := SetHomePin(p, "-home-me", "client-b", o); err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "another client repo") || strings.Contains(logs[0], "-home-me") {
		t.Fatalf("log: %v", logs)
	}
}
