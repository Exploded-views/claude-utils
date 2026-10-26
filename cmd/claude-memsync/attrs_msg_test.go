package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// A pulled .gitattributes that differs from the one pinned at init is part of
// the review, but it is not a key: the review must say which file changed, by
// which commit, how to read it, and that a first join expects it.
func TestRecipientsAccept_GitattributesChangeIsExplainedNotShownAsAKey(t *testing.T) {
	cfgs := routedHost(t, []string{"personal", "projects"}, "personal", nil)
	proj := encryptRouted(t, cfgs["projects"])
	repo := gitwt.New(proj.SyncDir)
	attrs := filepath.Join(proj.SyncDir, ".gitattributes")
	old, _ := os.ReadFile(attrs)
	if err := os.WriteFile(attrs, append(old, []byte("# a line a peer added\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := repo.AddAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("peer: touch attributes"); err != nil {
		t.Fatal(err)
	}
	h, err := repo.Run("rev-parse", "--short", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	h = strings.TrimSpace(h)

	code, _, errs := runRcpt("--repo", "projects", "accept")
	if code != 1 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if strings.Contains(errs, "+ key (a .gitattributes") || strings.Contains(errs, "(not pinned)") {
		t.Errorf("a .gitattributes change must not be listed as an unpinned key:\n%s", errs)
	}
	for _, want := range []string{
		"the repo's .gitattributes was changed by " + h,
		"git -C " + proj.SyncDir + " show " + h + " -- .gitattributes",
		"before accepting",
		"first join",
	} {
		if !strings.Contains(errs, want) {
			t.Errorf("missing %q in:\n%s", want, errs)
		}
	}
}

// An edit that was never committed has no commit to point at: say so and
// point at the diff instead.
func TestRecipientsAccept_UncommittedGitattributesChangeSaysSo(t *testing.T) {
	cfgs := routedHost(t, []string{"personal", "projects"}, "personal", nil)
	proj := encryptRouted(t, cfgs["projects"])
	attrs := filepath.Join(proj.SyncDir, ".gitattributes")
	old, _ := os.ReadFile(attrs)
	if err := os.WriteFile(attrs, append(old, []byte("# local edit\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, errs := runRcpt("--repo", "projects", "accept")
	if !strings.Contains(errs, "uncommitted") || !strings.Contains(errs, "git -C "+proj.SyncDir+" diff -- .gitattributes") {
		t.Errorf("uncommitted change not explained:\n%s", errs)
	}
	if strings.Contains(errs, "(not pinned)") {
		t.Errorf("listed as a key:\n%s", errs)
	}
}

// The library no longer smuggles the file change in as a pseudo key.
func TestRecipientsDiff_GitattributesIsNotAKey(t *testing.T) {
	cfgs := routedHost(t, []string{"personal", "projects"}, "personal", nil)
	proj := encryptRouted(t, cfgs["projects"])
	attrs := filepath.Join(proj.SyncDir, ".gitattributes")
	old, _ := os.ReadFile(attrs)
	if err := os.WriteFile(attrs, append(old, []byte("# x\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	added, removed, err := agecrypt.RecipientsDiff(gitwt.New(proj.SyncDir))
	if err != nil || len(added) != 0 || len(removed) != 0 {
		t.Fatalf("added=%v removed=%v err=%v", added, removed, err)
	}
}
