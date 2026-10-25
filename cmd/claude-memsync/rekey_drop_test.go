package main

import (
	"bytes"
	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRekeyPQ_DropBackupKeepsStashEntries(t *testing.T) {
	c := newClassicRepo(t)
	_, newRec := newPQKey(t, "newrec")
	if code, o, e := rekey(t, c, "--recovery-recipient", newRec, "--host", "vm-1", "--fresh-root", "--apply"); code != 0 {
		t.Fatalf("%s%s", o, e)
	}
	// a stash entry, as the daemon's autostash leaves
	notes := filepath.Join(c.cfg.SyncDir, "projects", "p1", "memory", "stashme.md")
	if err := os.WriteFile(notes, []byte("stash me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.repo.Run("add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.repo.Run("stash", "push", "-q"); err != nil {
		t.Fatal(err)
	}
	var o, e bytes.Buffer
	if code := recipientsCmd(&o, &e, []string{"rekey-pq", "--config", c.cfg.Path(), "--drop-backup"}); code != 0 {
		t.Fatalf("%s", e.String())
	}
	if l, _ := c.repo.Run("stash", "list"); strings.TrimSpace(l) == "" {
		t.Error("--drop-backup destroyed a stash entry")
	}
	if _, err := c.repo.Run("rev-parse", "--verify", "--quiet", agecrypt.BackupRef); err == nil {
		t.Error("backup ref should be gone")
	}
	if _, err := os.Stat(filepath.Join(c.cfg.SyncDir, ".git", "memsync-pre-pq.json")); err == nil {
		t.Error("the restore record should go with the backup")
	}
}
