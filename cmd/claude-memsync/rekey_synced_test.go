package main

import (
	syncpkg "github.com/MarimerLLC/claude-utils/internal/sync"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRekeyPQ_MovesTheSyncedHeadRecord(t *testing.T) {
	c := newClassicRepo(t)
	_, newRec := newPQKey(t, "newrec")
	tip, _ := c.repo.Run("rev-parse", "HEAD")
	rec := filepath.Join(c.cfg.SyncDir, ".state", "synced-head")
	if err := os.MkdirAll(filepath.Dir(rec), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rec, []byte(strings.TrimSpace(tip)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, o, e := rekey(t, c, "--recovery-recipient", newRec, "--host", "vm-1", "--fresh-root", "--apply"); code != 0 {
		t.Fatalf("%s%s", o, e)
	}
	head, _ := c.repo.Run("rev-parse", "HEAD")
	got, err := syncpkg.SyncedHead(c.repo)
	if err != nil || got != strings.TrimSpace(head) {
		t.Errorf("synced head %q, want the new HEAD %q (%v)", got, strings.TrimSpace(head), err)
	}
}
