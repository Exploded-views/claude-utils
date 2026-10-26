package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// pushFromElsewhere clones origin, runs fn there (git -c identity set) and
// pushes main, as another host would.
func pushFromElsewhere(t *testing.T, origin string, fn func(g func(args ...string))) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "other")
	r := gitwt.New(dir)
	if err := r.Clone(origin); err != nil {
		t.Fatal(err)
	}
	g := func(args ...string) {
		t.Helper()
		if _, err := r.Run(append([]string{"-c", "user.name=o", "-c", "user.email=o@example.com"}, args...)...); err != nil {
			t.Fatal(err)
		}
	}
	fn(g)
	g("push", "origin", "main")
}

// S-A: a classic commit another host pushed on top of the backup tip is not
// the migration. The inference must not take it for the migrated commit.
func TestLegacyRecord_AnotherHostsClassicCommitIsNotTheMigration(t *testing.T) {
	c := newClassicRepo(t)
	_, newRec := newPQKey(t, "newrec")
	args := []string{"--recovery-recipient", newRec, "--host", "vm-1", "--apply"}
	blockPush(t, c.origin)
	if code, o, e := rekey(t, c, args...); code == 0 {
		t.Fatalf("push was blocked yet exit 0\n%s%s", o, e)
	}
	unblockPush(t, c.origin)
	pushFromElsewhere(t, c.origin, func(g func(args ...string)) {
		g("commit", "-q", "--allow-empty", "-m", "classic commit of another host")
	})
	makeLegacyRecord(t, c.cfg.SyncDir)
	cfgBefore, _ := os.ReadFile(c.cfg.Path())
	recBefore, _ := os.ReadFile(filepath.Join(c.cfg.SyncDir, ".git", "memsync-pre-pq.json"))
	code, out, errb := rekey(t, c, args...)
	all := out + errb
	if code != 1 || !strings.Contains(all, "cannot tell which commit was the migration") || !strings.Contains(all, "nothing changed") {
		t.Fatalf("exit %d, want 1 with \"cannot tell which commit was the migration; nothing changed\":\n%s", code, all)
	}
	if recAfter, _ := os.ReadFile(filepath.Join(c.cfg.SyncDir, ".git", "memsync-pre-pq.json")); !bytes.Equal(recAfter, recBefore) {
		t.Error("the record was touched")
	}
	if cfgAfter, _ := os.ReadFile(c.cfg.Path()); !bytes.Equal(cfgAfter, cfgBefore) {
		t.Error("config.json was touched")
	}
	if _, blocked := agecrypt.RekeyBlocksSync(c.repo); !blocked {
		t.Error("the daemon was unblocked")
	}
	if code, out := dropBackup(t, c); code == 0 {
		t.Errorf("drop-backup went ahead:\n%s", out)
	}
}

// S-B: origin/main has more than one root (a merged unrelated history). The
// migrated root is still found, and the commit-count check is never reached.
func TestLegacyRecord_FreshRootWithAnotherRootOnOriginIsStillDone(t *testing.T) {
	c := newClassicRepo(t)
	_, newRec := newPQKey(t, "newrec")
	args := []string{"--recovery-recipient", newRec, "--host", "vm-1", "--fresh-root", "--apply"}
	if code, o, e := rekey(t, c, args...); code != 0 {
		t.Fatalf("%s%s", o, e)
	}
	pushFromElsewhere(t, c.origin, func(g func(args ...string)) {
		g("checkout", "-q", "--orphan", "unrelated")
		g("rm", "-rfq", "--ignore-unmatch", ".")
		g("commit", "-q", "--allow-empty", "-m", "second root")
		g("checkout", "-q", "main")
		g("merge", "-q", "--allow-unrelated-histories", "--no-edit", "unrelated")
	})
	makeLegacyRecord(t, c.cfg.SyncDir)
	cfgBefore, _ := os.ReadFile(c.cfg.Path())
	code, out, errb := rekey(t, c, args...)
	all := out + errb
	if code != 0 || !strings.Contains(all, "already done") || strings.Contains(all, "verification failed") {
		t.Fatalf("exit %d, want 0 and \"already done\":\n%s", code, all)
	}
	if cfgAfter, _ := os.ReadFile(c.cfg.Path()); !bytes.Equal(cfgAfter, cfgBefore) {
		t.Error("config.json was rewritten")
	}
}

// Nit: a rollback that only cleared a stale record must not claim a restore.
func TestRekeyPQ_OrphanRollbackSaysItOnlyClearedTheRecord(t *testing.T) {
	c, _ := crashedBeforeBackupRef(t)
	code, out, errb := rollbackCmd(t, c)
	if code != 0 || !strings.Contains(out, "cleared a stale restore record (nothing had changed)") || strings.Contains(out, "rolled back") {
		t.Errorf("exit %d:\n%s%s", code, out, errb)
	}
}

// Nit: the discard listing goes to the command's stderr writer.
func TestRekeyPQ_DiscardListsTheDroppedCommitsOnTheCommandStderr(t *testing.T) {
	c, _, _ := unpublishedRekey(t)
	commitEmpty(t, c.repo, "note to be dropped")
	code, _, errb := rollbackCmd(t, c, "--discard-local-commits")
	if code != 0 || !strings.Contains(errb, "note to be dropped") {
		t.Errorf("exit %d, stderr lacks the dropped commit:\n%s", code, errb)
	}
}

// Nit: a clone whose git config cannot be read is assumed to use the identity.
func TestRetireClassic_UnreadableCloneConfigKeepsTheIdentity(t *testing.T) {
	c := newClassicRepo(t)
	_, newRec := newPQKey(t, "newrec")
	sib := c.cfg.SyncDir + ".incident-3"
	sr := gitwt.New(sib)
	if err := sr.Init("main"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sib, ".git", "config"), []byte("[memage\n\tidentity = /elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errb := rekey(t, c, "--recovery-recipient", newRec, "--host", "vm-1", "--fresh-root", "--apply")
	if code != 0 {
		t.Fatalf("%d %s%s", code, out, errb)
	}
	if _, err := os.Stat(c.idPath); err != nil {
		t.Errorf("retired although a clone's config could not be read: %v", err)
	}
	if !strings.Contains(out, "incident-3") {
		t.Errorf("the hint must name the clone:\n%s", out)
	}
}
