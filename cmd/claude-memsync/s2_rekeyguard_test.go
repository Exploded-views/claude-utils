package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/lock"
)

// A half-migrated repo (rekey-pq applied, not published) must not be touched
// by an interactive mutator: with the auto stop, accepting a join would
// publish the unfinished migration.
func wantRekeyRefusal(t *testing.T, f *fakeSvc, errs string, ran bool) {
	t.Helper()
	if ran {
		t.Error("the change ran on a half-migrated repo")
	}
	if f.did() != "" {
		t.Errorf("the service was touched (%s); the refusal must come first", f.did())
	}
	for _, w := range []string{"finish or roll back the rekey first", "rekey-pq", "--apply", "--rollback"} {
		if !strings.Contains(errs, w) {
			t.Errorf("message lacks %q:\n%s", w, errs)
		}
	}
}

func TestSyncLockStopping_RefusesAHalfMigratedRepo(t *testing.T) {
	c, _, _ := unpublishedRekey(t)
	f := &fakeSvc{status: "running"}
	installFakeSvc(t, f)
	held, err := lock.Acquire(c.cfg.SyncDir) // the daemon holds it
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	f.onStop = held.Release
	var errb bytes.Buffer
	ran := false
	err = withSyncLockStopping(&errb, c.cfg.SyncDir, func() error { ran = true; return nil })
	if err == nil {
		t.Fatal("no refusal")
	}
	wantRekeyRefusal(t, f, err.Error(), ran)
	if !strings.Contains(err.Error(), filepath.Base(c.cfg.SyncDir)) {
		t.Errorf("the repo is not named: %v", err)
	}
}

func TestJoinAccept_RefusesAHalfMigratedRepoWithoutStoppingTheService(t *testing.T) {
	c, _, _ := unpublishedRekey(t)
	f := &fakeSvc{status: "running"}
	installFakeSvc(t, f)
	held, err := lock.Acquire(c.cfg.SyncDir)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	f.onStop = held.Release
	code, _, errs := runJoinCmd("accept", "new-host", "--config", c.cfg.Path(), "--fingerprint", "0000000000000000", "--allow-unlabelled")
	if code == 0 {
		t.Fatal("accept succeeded")
	}
	wantRekeyRefusal(t, f, errs, false)
}

func TestRecipientsAdd_RefusesAHalfMigratedRepoWithoutStoppingTheService(t *testing.T) {
	c, _, _ := unpublishedRekey(t)
	f := &fakeSvc{status: "running"}
	installFakeSvc(t, f)
	held, err := lock.Acquire(c.cfg.SyncDir)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	f.onStop = held.Release
	code, _, errs := runRcpt("--config", c.cfg.Path(), "add", newAgeKey(t))
	if code == 0 {
		t.Fatal("add succeeded")
	}
	wantRekeyRefusal(t, f, errs, false)
}

// A clone that cannot be brought up to date with origin must not accept or
// reject a join request from a stale view of the recipients.
func TestJoinAccept_DivergenceFromOriginIsFatal(t *testing.T) {
	h := twoHosts(t)
	_, out, _ := runJoinCmd("request", "--config", h.b.Path(), "--label", "new-host")
	fp := fpOf(out)
	if _, _, e := runJoinCmd("list", "--config", h.a.Path()); e != "" {
		t.Fatal(e)
	}
	if _, err := h.ra.Run("pull", "-q", "--rebase", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	// a local commit on a, and another commit on origin: diverged.
	oid, _ := h.ra.RunStdin([]byte("x\n"), "hash-object", "-w", "--no-filters", "--stdin")
	h.ra.Run("update-index", "--add", "--cacheinfo", "100644,"+strings.TrimSpace(oid)+",stray/file.txt")
	if _, err := h.ra.Run("commit", "-q", "-m", "local"); err != nil {
		t.Fatal(err)
	}
	pushToBare(t, h.origin, map[string]string{"elsewhere/file.txt": "y"})
	head, _ := h.ra.Run("rev-parse", "HEAD")
	for _, args := range [][]string{
		{"accept", "new-host", "--config", h.a.Path(), "--allow-unlabelled", "--fingerprint", fp},
		{"reject", "new-host", "--config", h.a.Path()},
	} {
		code, _, errs := runJoinCmd(args...)
		if code == 0 || !strings.Contains(errs, "diverged") {
			t.Fatalf("%s must fail on a divergence: %d %s", args[0], code, errs)
		}
		if now, _ := h.ra.Run("rev-parse", "HEAD"); now != head {
			t.Errorf("%s committed on a diverged clone", args[0])
		}
	}
	if !strings.Contains(originFiles(t, h.origin), "joins/new-host.txt") {
		t.Error("the request left origin")
	}
}
