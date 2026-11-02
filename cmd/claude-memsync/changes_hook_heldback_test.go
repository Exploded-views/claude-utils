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
	// Like the daemon: a "#denylist" line when a denylist existed, and the
	// notes themselves on disk.
	lines := paths
	if _, err := os.Stat(filepath.Join(syncDir, "denylist.txt")); err == nil {
		lines = append([]string{"#denylist"}, paths...)
	}
	for _, rel := range paths {
		if strings.HasPrefix(rel, "#") {
			continue
		}
		f := filepath.Join(syncDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
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

// The reviewer's probe: the denylist goes away after the daemon recorded a
// name that matches it. Without the denylist the hook cannot tell which names
// are the private term, so it names none.
func TestHook_NoNamesWhenTheDenylistIsGone(t *testing.T) {
	cfgPath, dir := heldFixture(t)
	in := hookJSON("sess-hb5", "/home/alice/src/app", "UserPromptSubmit")
	runHook(t, in, "--config", cfgPath, "--hook")
	writeHeldRecord(t, dir,
		"projects/"+ownFolder+"/memory/body.md",
		"projects/"+ownFolder+"/memory/Nightjar-plan.md")
	if err := os.Remove(filepath.Join(dir, "denylist.txt")); err != nil {
		t.Fatal(err)
	}
	_, out, _ := runHook(t, in, "--config", cfgPath, "--hook")
	ctx := hookContext(t, out)
	if strings.Contains(strings.ToLower(ctx), "nightjar") || strings.Contains(ctx, "body.md") {
		t.Errorf("names listed with the denylist gone:\n%s", ctx)
	}
	if !strings.Contains(ctx, "NOT syncing") || !strings.Contains(ctx, "not named") {
		t.Errorf("want a count without names:\n%s", ctx)
	}
}

// A repo that never had a denylist holds only secret-rule notes; those names
// are safe to list.
func TestHook_NamesListedWhenThereWasNeverADenylist(t *testing.T) {
	cfgPath, dir := heldFixture(t)
	if err := os.Remove(filepath.Join(dir, "denylist.txt")); err != nil {
		t.Fatal(err)
	}
	in := hookJSON("sess-hb6", "/home/alice/src/app", "UserPromptSubmit")
	runHook(t, in, "--config", cfgPath, "--hook")
	writeHeldRecord(t, dir, "projects/"+ownFolder+"/memory/leaky.md")
	_, out, _ := runHook(t, in, "--config", cfgPath, "--hook")
	if !strings.Contains(hookContext(t, out), "leaky.md") {
		t.Errorf("secret-held note not named:\n%s", hookContext(t, out))
	}
}

func TestHook_BlockedRecordSaysNothingIsCommitted(t *testing.T) {
	cfgPath, dir := heldFixture(t)
	in := hookJSON("sess-hb7", "/home/alice/src/app", "UserPromptSubmit")
	runHook(t, in, "--config", cfgPath, "--hook")
	writeHeldRecord(t, dir, "#blocked", "projects/"+ownFolder+"/memory/body.md")
	_, out, _ := runHook(t, in, "--config", cfgPath, "--hook")
	ctx := hookContext(t, out)
	if !strings.Contains(ctx, "memsync is committing nothing in this repo: its denylist is missing or unreadable") {
		t.Errorf("no blocked notice:\n%s", ctx)
	}
	if strings.Contains(ctx, "body.md") {
		t.Errorf("a blocked record names files:\n%s", ctx)
	}
}

func TestHook_CountsNameHeldNotesWithoutNamingThem(t *testing.T) {
	cfgPath, dir := heldFixture(t)
	in := hookJSON("sess-hb8", "/home/alice/src/app", "UserPromptSubmit")
	runHook(t, in, "--config", cfgPath, "--hook")
	writeHeldRecord(t, dir, "#name-held 2")
	_, out, _ := runHook(t, in, "--config", cfgPath, "--hook")
	ctx := hookContext(t, out)
	if !strings.Contains(ctx, "NOT syncing 2 ") || !strings.Contains(ctx, "not named") {
		t.Errorf("want a count of the name-matched notes:\n%s", ctx)
	}
}

// The daemon may be stopped: a note that is gone is not worth a notice.
func TestHook_SkipsHeldNotesThatNoLongerExist(t *testing.T) {
	cfgPath, dir := heldFixture(t)
	in := hookJSON("sess-hb9", "/home/alice/src/app", "UserPromptSubmit")
	runHook(t, in, "--config", cfgPath, "--hook")
	writeHeldRecord(t, dir,
		"projects/"+ownFolder+"/memory/kept.md",
		"projects/"+ownFolder+"/memory/gone.md")
	if err := os.Remove(filepath.Join(dir, "projects", ownFolder, "memory", "gone.md")); err != nil {
		t.Fatal(err)
	}
	_, out, _ := runHook(t, in, "--config", cfgPath, "--hook")
	ctx := hookContext(t, out)
	if !strings.Contains(ctx, "kept.md") || strings.Contains(ctx, "gone.md") {
		t.Errorf("want only the note that still exists:\n%s", ctx)
	}
}

// When the list empties and the same list comes back, the session hears again.
func TestHook_TellsAgainWhenAClearedListRecurs(t *testing.T) {
	cfgPath, dir := heldFixture(t)
	in := hookJSON("sess-hb10", "/home/alice/src/app", "UserPromptSubmit")
	runHook(t, in, "--config", cfgPath, "--hook")
	note := "projects/" + ownFolder + "/memory/body.md"
	writeHeldRecord(t, dir, note)
	if _, out, _ := runHook(t, in, "--config", cfgPath, "--hook"); !strings.Contains(hookContext(t, out), "body.md") {
		t.Fatal("first notice missing")
	}
	if err := os.Remove(filepath.Join(dir, ".state", "held-back")); err != nil {
		t.Fatal(err)
	}
	runHook(t, in, "--config", cfgPath, "--hook") // fixed: the marker is cleared
	writeHeldRecord(t, dir, note)
	if _, out, _ := runHook(t, in, "--config", cfgPath, "--hook"); !strings.Contains(hookContext(t, out), "body.md") {
		t.Error("a recurring list is not told again")
	}
}

// A record cut at the read cap ends in a partial path; it must not be taken
// for a name.
func TestHook_DropsACutOffLastLine(t *testing.T) {
	cfgPath, dir := heldFixture(t)
	in := hookJSON("sess-hb11", "/home/alice/src/app", "UserPromptSubmit")
	runHook(t, in, "--config", cfgPath, "--hook")
	prefix := "projects/" + ownFolder + "/memory/"
	if err := os.WriteFile(filepath.Join(dir, "projects", ownFolder, "memory", "cutnam"), []byte("x"), 0o600); err != nil {
		// the directory may not exist yet
		os.MkdirAll(filepath.Join(dir, "projects", ownFolder, "memory"), 0o700)
		os.WriteFile(filepath.Join(dir, "projects", ownFolder, "memory", "cutnam"), []byte("x"), 0o600)
	}
	target := heldBackRecordMax - len(prefix+"cutnam")
	var rec strings.Builder
	for rec.Len()+100 < target {
		rec.WriteString("projects/-x/memory/" + strings.Repeat("a", 20) + ".md\n")
	}
	rec.WriteString(strings.Repeat("p", target-rec.Len()-1) + "\n")
	rec.WriteString(prefix + "cutnam.md\n") // the cap lands inside this line
	p := filepath.Join(dir, ".state", "held-back")
	os.MkdirAll(filepath.Dir(p), 0o700)
	if err := os.WriteFile(p, []byte(rec.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	_, out, _ := runHook(t, in, "--config", cfgPath, "--hook")
	if ctx := hookContext(t, out); strings.Contains(ctx, "cutnam") {
		t.Errorf("a cut-off line was taken for a note:\n%s", ctx)
	}
}
