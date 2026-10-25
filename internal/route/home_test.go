package route

import "testing"

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
