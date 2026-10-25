package main

import (
	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRekeyPQ_KeepsTheClassicIdentityForAnyCloneUnderTheSyncRoot(t *testing.T) {
	c := newClassicRepo(t)
	_, newRec := newPQKey(t, "newrec")
	sib := c.cfg.SyncDir + ".incident-1"
	sr := gitwt.New(sib)
	if err := sr.Init("main"); err != nil {
		t.Fatal(err)
	}
	if err := sr.ConfigSet(agecrypt.IdentityConfigKey, filepath.ToSlash(c.idPath)); err != nil {
		t.Fatal(err)
	}
	code, out, e := rekey(t, c, "--recovery-recipient", newRec, "--host", "vm-1", "--fresh-root", "--apply")
	if code != 0 {
		t.Fatalf("%d %s%s", code, out, e)
	}
	if _, err := os.Stat(c.idPath); err != nil {
		t.Error("the classic identity was moved although an unrouted clone still uses it")
	}
	if !strings.Contains(out, "incident-1") {
		t.Errorf("the hint must name the clone:\n%s", out)
	}
}
