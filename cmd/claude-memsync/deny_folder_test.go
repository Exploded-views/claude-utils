package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/related"
	syncpkg "github.com/MarimerLLC/claude-utils/internal/sync"
)

const denyFolder = "-synth-deny-pinned" // synthetic: matched by the generic rule below it

// A deny pin listed first beats the generic rule after it: Decide denies, and
// no repo's mirror gets any memory file of the folder.
func TestDenyPinFirst_DecideDeniesAndMirrorCopiesNothing(t *testing.T) {
	cfgs := routedHost(t, []string{"personal", "projects"}, "personal", []map[string]string{
		{"folder": denyFolder, "deny": ""},
		{"folder": "-synth-*", "repo": "projects"},
	})
	// "deny" is a bool in routes.json; rewrite the fixture's first rule
	p := os.Getenv(routesEnv)
	var rj map[string]any
	b, _ := os.ReadFile(p)
	if err := json.Unmarshal(b, &rj); err != nil {
		t.Fatal(err)
	}
	rj["routes"] = []any{
		map[string]any{"folder": denyFolder, "deny": true},
		map[string]any{"folder": "-synth-*", "repo": "projects"},
	}
	b, _ = json.Marshal(rj)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	claude := cfgs["projects"].ClaudeProjectsDir
	writeSource(t, claude, denyFolder, "secret")
	writeSource(t, claude, "-synth-ok", "fine")
	set, err := loadRouted(p)
	if err != nil {
		t.Fatal(err)
	}
	if d := set.Router.Decide(denyFolder); d.Repo != "" || d.Rule != 0 {
		t.Fatalf("deny pin did not win: %+v", d)
	}
	if d := set.Router.Decide("-synth-ok"); d.Repo != "projects" {
		t.Fatalf("generic rule lost: %+v", d)
	}
	for _, name := range []string{"personal", "projects"} {
		mirror := t.TempDir()
		roots := syncpkg.Roots{Claude: claude, Mirror: mirror, Owns: set.Router.Owns(name)}
		if _, err := syncpkg.Reconcile(roots, syncpkg.NewManifest()); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(mirror, denyFolder)); err == nil {
			t.Fatalf("%s: mirror holds the denied folder", name)
		}
		_, err := os.Stat(filepath.Join(mirror, "-synth-ok", "memory", "fine.md"))
		if (err == nil) != (name == "projects") {
			t.Fatalf("%s: owned folder mirrored=%v", name, err == nil)
		}
	}
}

// related suggest never reads a denied folder's transcripts, and still reads a
// routed one's.
func TestSuggest_DeniedFolderTranscriptsAreNeverOpened(t *testing.T) {
	h := fiveRepoHost(t)
	h.coOpen(t, "a", "c")
	p := os.Getenv(routesEnv)
	var rj map[string]any
	b, _ := os.ReadFile(p)
	if err := json.Unmarshal(b, &rj); err != nil {
		t.Fatal(err)
	}
	rj["routes"] = append([]any{
		map[string]any{"folder": denyFolder, "deny": true},
		map[string]any{"folder": "-synth-*", "repo": "a"},
	}, rj["routes"].([]any)...)
	b, _ = json.Marshal(rj)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	projects := filepath.Join(h.home, ".claude", "projects")
	dd := filepath.Join(projects, denyFolder)
	if err := os.MkdirAll(dd, 0o700); err != nil {
		t.Fatal(err)
	}
	line := `{"timestamp":"2026-10-02T09:00:00Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(dd, "s.jsonl"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	var dirs []string
	orig := updateSessions
	updateSessions = func(ix *related.SessionIndex, dir, folder string, dl time.Time) ([]related.Session, bool) {
		dirs = append(dirs, folder)
		return orig(ix, dir, folder, dl)
	}
	defer func() { updateSessions = orig }()
	refresh(t, time.Minute)
	seen := map[string]bool{}
	for _, f := range dirs {
		seen[f] = true
	}
	if seen[denyFolder] {
		t.Fatal("a denied folder's transcripts were read")
	}
	if !seen[h.folder["a"]] || !seen[h.folder["c"]] {
		t.Fatalf("routed folders not read: %v", dirs)
	}
}
