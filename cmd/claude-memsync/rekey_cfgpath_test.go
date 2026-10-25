package main

import (
	"bytes"
	"github.com/MarimerLLC/claude-utils/internal/config"
	"os"
	"path/filepath"
	"testing"
)

func TestRekeyPQ_SavesTheConfigFileItWasGiven(t *testing.T) {
	c := newClassicRepo(t)
	_, newRec := newPQKey(t, "newrec")
	alt := filepath.Join(t.TempDir(), "alt-config.json")
	b, _ := os.ReadFile(c.cfg.Path())
	if err := os.WriteFile(alt, b, 0o600); err != nil {
		t.Fatal(err)
	}
	var o, e bytes.Buffer
	code := recipientsCmd(&o, &e, []string{"rekey-pq", "--config", alt, "--recovery-recipient", newRec, "--host", "vm-1", "--fresh-root", "--apply"})
	if code != 0 {
		t.Fatalf("%d %s%s", code, o.String(), e.String())
	}
	got, _ := config.Load(alt)
	if got.EncryptionScheme != "pq" {
		t.Error("the config file named by --config was not updated")
	}
	if after, _ := os.ReadFile(c.cfg.Path()); !bytes.Equal(after, b) {
		t.Error("SyncDir/config.json was rewritten instead")
	}
}
