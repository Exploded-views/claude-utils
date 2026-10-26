package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
)

// relatedRekeyHost routes c's repo and gives the host a related root holding
// one encrypted read clone whose recipients are rcpts.
func relatedRekeyHost(t *testing.T, c classicRepo, rcpts ...string) string {
	t.Helper()
	home, _ := os.UserHomeDir()
	root := filepath.Join(home, "related-clones")
	clone := filepath.Join(root, "mem-other")
	if err := os.MkdirAll(clone, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clone, ".age-recipients"), []byte(strings.Join(rcpts, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rj := map[string]any{
		"repos":       map[string]any{"main": map[string]any{"config": c.cfg.Path(), "kind": "personal"}},
		"routes":      []any{},
		"default":     "deny",
		"personal":    "main",
		"relatedRoot": root,
	}
	b, _ := json.Marshal(rj)
	p := filepath.Join(home, ".config", "claude-memsync", "routes.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(routesEnv, p)
	return clone
}

// A related read clone decrypts with the host's default identity; while its
// repo still lists the classic recipient, retiring that identity would blind it.
func TestRekeyPQ_KeepsTheClassicIdentityForARelatedReadClone(t *testing.T) {
	c := newClassicRepo(t)
	classicRcpt, err := agecrypt.IdentityRecipient(c.idPath)
	if err != nil {
		t.Fatal(err)
	}
	relatedRekeyHost(t, c, classicRcpt+" # this host")
	_, newRec := newPQKey(t, "newrec")
	code, out, e := rekey(t, c, "--recovery-recipient", newRec, "--host", "vm-1", "--fresh-root", "--apply")
	if code != 0 {
		t.Fatalf("%d %s%s", code, out, e)
	}
	if _, err := os.Stat(c.idPath); err != nil {
		t.Error("the classic identity was retired although a related read clone still lists it")
	}
	if !strings.Contains(out, "mem-other") || !strings.Contains(out, "kept") {
		t.Errorf("the output must name the related clone and say the file is kept:\n%s", out)
	}
	// retire-classic says the same
	code2, out2, e2 := runRcpt("retire-classic", "--config", c.cfg.Path(), "--apply")
	if code2 != 0 || !strings.Contains(out2, "mem-other") {
		t.Errorf("retire-classic must keep it and name the clone: %d\n%s%s", code2, out2, e2)
	}
	if _, err := os.Stat(c.idPath); err != nil {
		t.Error("retire-classic --apply retired the identity")
	}
}

func TestRekeyPQ_RetiresTheClassicIdentityWhenNoRelatedCloneNeedsIt(t *testing.T) {
	c := newClassicRepo(t)
	_, other := newPQKey(t, "someone-else")
	relatedRekeyHost(t, c, other)
	_, newRec := newPQKey(t, "newrec")
	code, out, e := rekey(t, c, "--recovery-recipient", newRec, "--host", "vm-1", "--fresh-root", "--apply")
	if code != 0 {
		t.Fatalf("%d %s%s", code, out, e)
	}
	if _, err := os.Stat(c.idPath); err == nil {
		t.Errorf("the classic identity should be retired when no clone needs it:\n%s", out)
	}
}
