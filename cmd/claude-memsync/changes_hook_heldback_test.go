package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	ownFolder   = "-home-alice-src-app"
	otherFolder = "-home-alice-src-other"
)

// writeHeldRecord is what the daemon leaves after a commit that held notes
// back: repo-relative paths, one per line.
func writeHeldRecord(t *testing.T, syncDir string, paths ...string) {
	t.Helper()
	p := filepath.Join(syncDir, ".state", "held-back")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Join(paths, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func heldFixture(t *testing.T) (cfgPath, syncDir string) {
	t.Helper()
	cfgPath, _ = changesRepo(t)
	cfg, _ := loadDistillConfig(cfgPath)
	ignoreState(t, cfg.SyncDir)
	if err := os.WriteFile(cfg.DenylistPath(), []byte("nightjar\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, cfg.SyncDir
}

func TestHook_NamesHeldNotesOfTheSessionsProject(t *testing.T) {
	cfgPath, dir := heldFixture(t)
	in := hookJSON("sess-hb", "/home/alice/src/app", "UserPromptSubmit")
	runHook(t, in, "--config", cfgPath, "--hook") // baseline
	writeHeldRecord(t, dir,
		"projects/"+ownFolder+"/memory/body.md",
		"projects/"+ownFolder+"/memory/other-body.md",
		"projects/"+otherFolder+"/memory/theirs.md")

	_, out, errs := runHook(t, in, "--config", cfgPath, "--hook")
	ctx := hookContext(t, out)
	if !strings.Contains(ctx, "NOT syncing") || !strings.Contains(ctx, "body.md") || !strings.Contains(ctx, "other-body.md") {
		t.Fatalf("notice does not name the held notes (stderr %q):\n%s", errs, ctx)
	}
	if strings.Contains(ctx, "theirs.md") || strings.Contains(ctx, otherFolder) {
		t.Errorf("another project's held note is named:\n%s", ctx)
	}
	// Told once per session, again when the list changes.
	_, out, _ = runHook(t, in, "--config", cfgPath, "--hook")
	if strings.Contains(hookContext(t, out), "NOT syncing") {
		t.Errorf("same list told twice in one session:\n%s", hookContext(t, out))
	}
	writeHeldRecord(t, dir, "projects/"+ownFolder+"/memory/third.md")
	_, out, _ = runHook(t, in, "--config", cfgPath, "--hook")
	if !strings.Contains(hookContext(t, out), "third.md") {
		t.Errorf("a changed list is not told:\n%s", hookContext(t, out))
	}
}

func TestHook_HeldNoteInAnotherProjectIsNotNamed(t *testing.T) {
	cfgPath, dir := heldFixture(t)
	in := hookJSON("sess-hb2", "/home/alice/src/app", "UserPromptSubmit")
	runHook(t, in, "--config", cfgPath, "--hook")
	writeHeldRecord(t, dir, "projects/"+otherFolder+"/memory/theirs.md")
	_, out, _ := runHook(t, in, "--config", cfgPath, "--hook")
	if out != "" {
		t.Errorf("nothing of this project is held, but the hook said:\n%s", hookContext(t, out))
	}
}

// A name that matches the denylist is the term itself: counted, never listed.
func TestHook_HeldNoticeNeverCarriesTheMatchedTerm(t *testing.T) {
	cfgPath, dir := heldFixture(t)
	in := hookJSON("sess-hb3", "/home/alice/src/app", "UserPromptSubmit")
	runHook(t, in, "--config", cfgPath, "--hook")
	writeHeldRecord(t, dir,
		"projects/"+ownFolder+"/memory/body.md",
		"projects/"+ownFolder+"/memory/Nightjar-plan.md")
	_, out, _ := runHook(t, in, "--config", cfgPath, "--hook")
	ctx := hookContext(t, out)
	if !strings.Contains(ctx, "body.md") || !strings.Contains(ctx, "1 more") {
		t.Errorf("want body.md named and the name-matching note counted:\n%s", ctx)
	}
	if strings.Contains(strings.ToLower(ctx), "nightjar") {
		t.Errorf("notice carries the denylisted term:\n%s", ctx)
	}
}

// Reading the daemon's record costs no git process.
func TestHook_HeldNoticeAddsNoGitSpawns(t *testing.T) {
	cfgPath, dir := heldFixture(t)
	in := hookJSON("sess-hb4", "/home/alice/src/app", "UserPromptSubmit")
	runHook(t, in, "--config", cfgPath, "--hook") // baseline
	s := gitShim(t, "")
	seam0, shim0, _, _ := countedHook(t, s, in, "--config", cfgPath, "--deadline", "0")

	writeHeldRecord(t, dir, "projects/"+ownFolder+"/memory/body.md")
	seam1, shim1, out, _ := countedHook(t, s, in, "--config", cfgPath, "--deadline", "0")
	if !strings.Contains(hookContext(t, out), "body.md") {
		t.Fatalf("no notice:\n%s", out)
	}
	if seam1 != seam0 || shim1 != shim0 {
		t.Errorf("git processes with a held note: seam %d shim %d; without: seam %d shim %d", seam1, shim1, seam0, shim0)
	}
}
