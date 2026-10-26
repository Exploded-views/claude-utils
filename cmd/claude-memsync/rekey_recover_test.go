package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// commitEmpty makes an empty commit with the given subject, as the daemon's
// next cycle would after a rekey.
func commitEmpty(t *testing.T, r *gitwt.Repo, subject string) {
	t.Helper()
	if _, err := r.Run("-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", subject); err != nil {
		t.Fatal(err)
	}
}

// S1: after the daemon synced a rekeyed repo, rerunning the same command is a
// no-op that reports "already done", not a failed verification.
func TestRekeyPQ_RerunAfterTheDaemonSyncedIsAlreadyDone(t *testing.T) {
	c := newClassicRepo(t)
	_, newRec := newPQKey(t, "newrec")
	args := []string{"--recovery-recipient", newRec, "--host", "vm-1", "--fresh-root", "--apply"}
	if code, o, e := rekey(t, c, args...); code != 0 {
		t.Fatalf("%s%s", o, e)
	}
	commitEmpty(t, c.repo, "auto: daemon sync after the rekey")
	if err := c.repo.Push("main"); err != nil {
		t.Fatal(err)
	}
	code, out, errb := rekey(t, c, args...)
	if code != 0 {
		t.Fatalf("rerun after a sync: exit %d\n%s%s", code, out, errb)
	}
	if !strings.Contains(out, "already done") {
		t.Errorf("rerun must report \"already done\":\n%s", out)
	}
}

func TestRekeyPQ_AllDoesNotStopOnARepoTheDaemonSynced(t *testing.T) {
	cfgs := rekeyHost(t, "a", "b")
	_, rec := newPQKey(t, "rec")
	args := []string{"rekey-pq", "--all", "--recovery-recipient", rec, "--host", "vm-1", "--fresh-root", "--apply"}
	if code, o, e := runRcpt(args...); code != 0 {
		t.Fatalf("%s%s", o, e)
	}
	ra := gitwt.New(cfgs["a"].SyncDir)
	commitEmpty(t, ra, "auto: daemon sync")
	if err := ra.Push("main"); err != nil {
		t.Fatal(err)
	}
	code, out, errb := runRcpt(args...)
	if code != 0 {
		t.Fatalf("rerun --all: exit %d\n%s%s", code, out, errb)
	}
	if strings.Count(out, "already done") != 2 {
		t.Errorf("both repos are done:\n%s%s", out, errb)
	}
}

// retire-classic: a standalone command, no rekey flags.
func TestRecipientsRetireClassic_RetiresOnceNoCloneUsesIt(t *testing.T) {
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
	if code, o, e := rekey(t, c, "--recovery-recipient", newRec, "--host", "vm-1", "--fresh-root", "--apply"); code != 0 {
		t.Fatalf("%s%s", o, e)
	}
	if _, err := os.Stat(c.idPath); err != nil {
		t.Fatal("setup: the classic identity must be kept while the clone uses it")
	}
	// Still used: kept, and the clone is named.
	code, out, errb := runRcpt("retire-classic", "--config", c.cfg.Path(), "--apply")
	if code != 0 || !strings.Contains(out, "incident-1") {
		t.Fatalf("exit %d, the clone must be named:\n%s%s", code, out, errb)
	}
	if _, err := os.Stat(c.idPath); err != nil {
		t.Fatal("retired although a clone still uses it")
	}
	if err := os.RemoveAll(sib); err != nil {
		t.Fatal(err)
	}
	// A dry run changes nothing.
	if code, out, errb := runRcpt("retire-classic", "--config", c.cfg.Path()); code != 0 || !strings.Contains(out+errb, "dry run") {
		t.Fatalf("dry run: exit %d\n%s%s", code, out, errb)
	}
	if _, err := os.Stat(c.idPath); err != nil {
		t.Fatal("a dry run retired the identity")
	}
	if code, out, errb := runRcpt("retire-classic", "--config", c.cfg.Path(), "--apply"); code != 0 {
		t.Fatalf("apply: exit %d\n%s%s", code, out, errb)
	}
	if _, err := os.Stat(c.idPath + ".classic-retired"); err != nil {
		t.Errorf("not retired: %v", err)
	}
	if _, err := os.Stat(c.idPath); err == nil {
		t.Error("the classic file is still in place")
	}
}
