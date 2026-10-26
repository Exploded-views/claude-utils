package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
)

// A related clone that is a symlink still decrypts with the host's identity:
// the scan must not mistake it for "not a clone" and let the file be retired.
func TestRelatedCloneDirs_ASymlinkedCloneCountsAndKeepsTheIdentity(t *testing.T) {
	c := newClassicRepo(t)
	classicRcpt, err := agecrypt.IdentityRecipient(c.idPath)
	if err != nil {
		t.Fatal(err)
	}
	clone := relatedRekeyHost(t, c, "age1someoneelse")
	root := filepath.Dir(clone)
	real := filepath.Join(t.TempDir(), "real-clone")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, ".age-recipients"), []byte(classicRcpt+" # this host\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "mem-linked")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("cannot symlink here:", err)
	}
	var found bool
	for _, d := range relatedCloneDirs(repoSel{}).dirs {
		if d == link {
			found = true
		}
	}
	if !found {
		t.Error("the symlinked clone was skipped by the scan")
	}
	code, out, errs := runRcpt("retire-classic", "--config", c.cfg.Path(), "--apply")
	if code != 0 || !strings.Contains(out, "mem-linked") || !strings.Contains(out, "kept") {
		t.Errorf("retire-classic must keep the identity and name the linked clone: %d\n%s%s", code, out, errs)
	}
	if _, err := os.Stat(c.idPath); err != nil {
		t.Error("the identity was retired although a symlinked clone lists it")
	}
}

// A marker that cannot be examined (anything but "does not exist") means the
// clone cannot be ruled out: it counts, and the identity is kept.
func TestRelatedCloneDirs_AnUnexaminableMarkerCountsAsACloneUsingTheIdentity(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a permission the user can lose")
	}
	c := newClassicRepo(t)
	clone := relatedRekeyHost(t, c, "age1someoneelse")
	if err := os.Chmod(clone, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(clone, 0o700) })
	var found bool
	for _, d := range relatedCloneDirs(repoSel{}).dirs {
		if d == clone {
			found = true
		}
	}
	if !found {
		t.Error("a clone whose marker could not be examined was treated as not a clone")
	}
	if got := relatedClonesUsingIdentity(c.idPath, []string{clone}); len(got) != 1 {
		t.Errorf("an unreadable clone must count as using the identity: %v", got)
	}
}
