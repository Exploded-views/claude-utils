package main

import (
	"bytes"
	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRekeyPQ_AllStopsAtFirstFailureAndRerunResumes(t *testing.T) {
	cfgs := rekeyHost(t, "a", "b", "c")
	_, rec := newPQKey(t, "rec")
	orig := map[string]map[string]string{}
	oldIDs := map[string]string{}
	for n, c := range cfgs {
		orig[n] = plainNotes(t, c.SyncDir)
		if len(orig[n]) == 0 {
			t.Fatalf("%s has no notes", n)
		}
		oldIDs[n] = agecrypt.IdentityPath(gitwt.New(c.SyncDir))
	}
	cTip := originTip(t, cfgs["c"].RemoteURL)
	blockPush(t, cfgs["b"].RemoteURL)
	args := []string{"rekey-pq", "--all", "--recovery-recipient", rec, "--host", "vm-1", "--fresh-root", "--apply"}
	code, out, errb := runRcpt(args...)
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s%s", code, out, errb)
	}
	all := out + errb
	for _, want := range []string{"done: a", "failed: b", "not started: c"} {
		if !strings.Contains(all, want) {
			t.Errorf("summary lacks %q:\n%s", want, all)
		}
	}
	// c was never touched.
	cr := gitwt.New(cfgs["c"].SyncDir)
	if agecrypt.SchemeOf(cr) != agecrypt.SchemeClassic {
		t.Error("c was migrated although the run had stopped")
	}
	if _, err := cr.Run("rev-parse", "--verify", "--quiet", agecrypt.BackupRef); err == nil {
		t.Error("c has a backup ref")
	}
	if originTip(t, cfgs["c"].RemoteURL) != cTip {
		t.Error("c's origin moved")
	}
	// b: the failed push left a migrated clone, its config still classic.
	bc, _ := config.Load(cfgs["b"].Path())
	if bc.EncryptionScheme == "pq" {
		t.Error("b's config.json was saved before its push succeeded")
	}
	// Rerun after lifting the block: resumes and finishes all three.
	unblockPush(t, cfgs["b"].RemoteURL)
	code, out, errb = runRcpt(args...)
	if code != 0 {
		t.Fatalf("rerun exit %d\n%s%s", code, out, errb)
	}
	for n, c := range cfgs {
		r := gitwt.New(c.SyncDir)
		if agecrypt.SchemeOf(r) != agecrypt.SchemePQ {
			t.Errorf("%s: not pq", n)
		}
		cc, _ := config.Load(c.Path())
		if cc.EncryptionScheme != "pq" || cc.HostLabel != "vm-1" {
			t.Errorf("%s: config %+v", n, cc)
		}
		if cnt, _ := gitwt.New(c.RemoteURL).Run("rev-list", "--count", "main"); strings.TrimSpace(cnt) != "1" {
			t.Errorf("%s: origin has %s commits", n, cnt)
		}
		newID := newIdentityPath(oldIDs[n])
		got := originNotes(t, c.RemoteURL, newID)
		for p, want := range orig[n] {
			if got[p] != want {
				t.Errorf("%s: %s differs after rekey: %q != %q", n, p, got[p], want)
			}
		}
		if len(got) != len(orig[n]) {
			t.Errorf("%s: %d notes on origin, want %d", n, len(got), len(orig[n]))
		}
		now := plainNotes(t, c.SyncDir)
		for p, want := range orig[n] {
			if now[p] != want {
				t.Errorf("%s: work-tree %s changed", n, p)
			}
		}
		if _, err := os.Stat(oldIDs[n] + ".classic-retired"); err != nil {
			t.Errorf("%s: classic identity not retired: %v", n, err)
		}
	}
}

func TestRekeyPQ_RollbackRestoresAWorkingClassicRepo(t *testing.T) {
	c := newClassicRepo(t)
	_, newRec := newPQKey(t, "newrec")
	oldTip, _ := c.repo.Run("rev-parse", "HEAD")
	oldTip = strings.TrimSpace(oldTip)
	cfgBefore, _ := os.ReadFile(c.cfg.Path())
	notes := plainNotes(t, c.cfg.SyncDir)
	remoteBefore := originTip(t, c.origin)
	blockPush(t, c.origin)
	code, out, errb := rekey(t, c, "--recovery-recipient", newRec, "--host", "vm-1", "--fresh-root", "--apply")
	if code == 0 {
		t.Fatalf("push was blocked yet exit 0\n%s", out)
	}
	if !strings.Contains(errb, "push") {
		t.Errorf("a push failure must say so: %s", errb)
	}
	st := filepath.Join(c.cfg.SyncDir, ".git", "memsync-pre-pq.json")
	fi, err := os.Stat(st)
	if err != nil {
		t.Fatalf("no restore record: %v", err)
	}
	if runtimeUnixPerm(fi) != 0o600 {
		t.Errorf("record mode %v", fi.Mode())
	}
	rb, _ := os.ReadFile(st)
	if strings.Contains(string(rb), "AGE-SECRET-KEY") {
		t.Error("the record holds a secret")
	}
	// the pushed remote is unchanged, so a rollback is allowed
	var o2, e2 bytes.Buffer
	if code := recipientsCmd(&o2, &e2, []string{"rekey-pq", "--config", c.cfg.Path(), "--rollback"}); code != 0 {
		t.Fatalf("rollback: %d %s%s", code, o2.String(), e2.String())
	}
	if agecrypt.SchemeOf(c.repo) != agecrypt.SchemeClassic {
		t.Error("scheme not restored")
	}
	if got := agecrypt.IdentityPath(c.repo); got != filepath.ToSlash(c.idPath) {
		t.Errorf("identity %q, want %q", got, c.idPath)
	}
	if h, _ := c.repo.Run("rev-parse", "HEAD"); strings.TrimSpace(h) != oldTip {
		t.Error("HEAD not back at the old tip")
	}
	if _, err := c.repo.Run("rev-parse", "--verify", "--quiet", agecrypt.BackupRef); err == nil {
		t.Error("backup ref should be gone")
	}
	if _, err := os.Stat(st); err == nil {
		t.Error("record should be gone")
	}
	if cfgAfter, _ := os.ReadFile(c.cfg.Path()); !bytes.Equal(cfgAfter, cfgBefore) {
		t.Errorf("config.json differs after rollback:\n%s\nvs\n%s", cfgAfter, cfgBefore)
	}
	if u, _ := agecrypt.Unreviewed(c.repo); u {
		t.Error("pin not restored")
	}
	if err := agecrypt.CheckKeyed(c.repo); err != nil {
		t.Errorf("classic repo no longer decrypts: %v", err)
	}
	if d, _ := c.repo.Run("status", "--porcelain"); strings.TrimSpace(d) != "" {
		t.Errorf("dirty: %s", d)
	}
	got := plainNotes(t, c.cfg.SyncDir)
	for p, w := range notes {
		if got[p] != w {
			t.Errorf("note %s changed by the round trip", p)
		}
	}
	if originTip(t, c.origin) != remoteBefore {
		t.Error("origin moved")
	}
	// and it can rekey again afterwards
	unblockPush(t, c.origin)
	if code, o, e := rekey(t, c, "--recovery-recipient", newRec, "--host", "vm-1", "--fresh-root", "--apply"); code != 0 {
		t.Fatalf("rekey after rollback: %d %s%s", code, o, e)
	}
}

func TestRekeyPQ_RollbackRefusedOnceTheRemoteMoved(t *testing.T) {
	c := newClassicRepo(t)
	_, newRec := newPQKey(t, "newrec")
	if code, o, e := rekey(t, c, "--recovery-recipient", newRec, "--host", "vm-1", "--fresh-root", "--apply"); code != 0 {
		t.Fatalf("%s%s", o, e)
	}
	head, _ := c.repo.Run("rev-parse", "HEAD")
	var o2, e2 bytes.Buffer
	if code := recipientsCmd(&o2, &e2, []string{"rekey-pq", "--config", c.cfg.Path(), "--rollback"}); code == 0 || !strings.Contains(e2.String(), "remote") {
		t.Fatalf("rollback after the push: %d %s", code, e2.String())
	}
	if h2, _ := c.repo.Run("rev-parse", "HEAD"); h2 != head || agecrypt.SchemeOf(c.repo) != agecrypt.SchemePQ {
		t.Error("a refused rollback changed the repo")
	}
}

func TestRekeyPQ_RerunFinishesACrashBetweenHeadMoveAndRepoint(t *testing.T) {
	c := newClassicRepo(t)
	_, newRec := newPQKey(t, "newrec")
	newID := newIdentityPath(c.idPath)
	if _, err := agecrypt.ApplyRekeyPQ(c.repo, agecrypt.RekeyOpts{
		Label: "vm-1", Recovery: newRec, ConfiguredRecovery: c.cfg.RecoveryRecipients, FreshRoot: true, NewIdentityPath: newID,
	}); err != nil {
		t.Fatal(err)
	}
	// the crash: identity, scheme and (maybe) pin were not repointed yet
	if err := c.repo.ConfigSet(agecrypt.IdentityConfigKey, filepath.ToSlash(c.idPath)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.repo.Run("config", "--unset", agecrypt.SchemeConfigKey); err != nil {
		t.Fatal(err)
	}
	if code, o, e := rekey(t, c, "--recovery-recipient", newRec, "--host", "vm-1", "--fresh-root", "--apply"); code != 0 {
		t.Fatalf("resume: %d %s%s", code, o, e)
	}
	if agecrypt.SchemeOf(c.repo) != agecrypt.SchemePQ || agecrypt.IdentityPath(c.repo) != filepath.ToSlash(newID) {
		t.Error("scheme or identity not repointed")
	}
	if u, _ := agecrypt.Unreviewed(c.repo); u {
		t.Error("pin not set")
	}
	if n, _ := gitwt.New(c.origin).Run("rev-list", "--count", "main"); strings.TrimSpace(n) != "1" {
		t.Errorf("origin has %s commits", n)
	}
	if cc, _ := config.Load(c.cfg.Path()); cc.EncryptionScheme != "pq" {
		t.Error("config not saved")
	}
	if err := agecrypt.CheckKeyed(c.repo); err != nil {
		t.Error(err)
	}
}

func TestRekeyPQ_RefusalsComeBeforeAnyLocalChange(t *testing.T) {
	c := newClassicRepo(t)
	_, newRec := newPQKey(t, "newrec")
	// private-only with a remote the guard refuses: nothing may change locally
	cc, _ := config.Load(c.cfg.Path())
	cc.PrivateOnly = true
	cc.RemoteURL = "git@evil.example.com:x/mem-y.git"
	if err := config.Save(cc); err != nil {
		t.Fatal(err)
	}
	before := c.state(t)
	if code, _, e := rekey(t, c, "--recovery-recipient", newRec, "--host", "vm-1", "--fresh-root", "--apply"); code == 0 {
		t.Fatalf("expected refusal: %s", e)
	}
	if after := c.state(t); after != before {
		t.Error("a refused rekey changed the repo")
	}
}

func TestRekeyPQ_AllDryRunSummarisesAndFailsIfAnyWouldFail(t *testing.T) {
	cfgs := rekeyHost(t, "a", "b", "c")
	_, rec := newPQKey(t, "rec")
	args := []string{"rekey-pq", "--all", "--recovery-recipient", rec, "--host", "vm-1", "--fresh-root"}

	code, out, errb := runRcpt(args...)
	if code != 0 || !strings.Contains(out+errb, "dry run: 3 ok, 0 would fail") {
		t.Fatalf("clean dry run: exit %d\n%s%s", code, out, errb)
	}

	// Break b: the dry run goes on past it, then says so and exits nonzero.
	if err := os.RemoveAll(filepath.Join(cfgs["b"].SyncDir, ".git")); err != nil {
		t.Fatal(err)
	}
	code, out, errb = runRcpt(args...)
	if code == 0 {
		t.Fatalf("a dry run with a failing repo must exit nonzero\n%s%s", out, errb)
	}
	all := out + errb
	if !strings.Contains(all, "dry run: 2 ok, 1 would fail") {
		t.Errorf("summary lacks the dry run line:\n%s", all)
	}
	if !strings.HasSuffix(strings.TrimSpace(errb), "dry run: 2 ok, 1 would fail") {
		t.Errorf("the dry run line must end the summary:\n%s", errb)
	}
	if !strings.Contains(out, cfgs["c"].SyncDir) {
		t.Errorf("c was not planned after b failed:\n%s", out)
	}
}
