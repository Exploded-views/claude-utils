package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/config"
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

// The new set is always exactly the new hybrid key labelled with --host and the
// recovery key labelled "# recovery", whatever labels the old set had.
func TestRekeyPQ_NewRecipientsAreLabelledFromTheFlags(t *testing.T) {
	for _, fresh := range []bool{true, false} {
		cfgs := rekeyHost(t, "alpha", "beta")
		// the old sets carry no labels at all
		for n, c := range cfgs {
			unlabel(t, c)
			_ = n
		}
		_, rec := newPQKey(t, "rec")
		args := []string{"rekey-pq", "--all", "--recovery-recipient", rec, "--host", "rk", "--apply"}
		if fresh {
			args = append(args, "--fresh-root")
		}
		if code, o, e := runRcpt(args...); code != 0 {
			t.Fatalf("fresh=%v: %s%s", fresh, o, e)
		}
		for n, c := range cfgs {
			og := gitwt.New(c.RemoteURL)
			got, err := og.Run("show", "main:.age-recipients")
			if err != nil {
				t.Fatal(err)
			}
			var keys []string
			for _, l := range strings.Split(got, "\n") {
				if strings.HasPrefix(l, "age1pq1") {
					keys = append(keys, l)
				}
			}
			if len(keys) != 2 {
				t.Fatalf("fresh=%v %s: %d keys on origin:\n%s", fresh, n, len(keys), got)
			}
			var host, recov int
			for _, k := range keys {
				if strings.HasSuffix(k, " # host:rk") {
					host++
				}
				if k == rec+" # recovery" {
					recov++
				}
			}
			if host != 1 || recov != 1 {
				t.Errorf("fresh=%v %s: labels wrong on origin:\n%s", fresh, n, got)
			}
			work, _ := os.ReadFile(filepath.Join(c.SyncDir, ".age-recipients"))
			if string(work) != got && strings.TrimSpace(string(work)) != strings.TrimSpace(got) {
				t.Errorf("fresh=%v %s: work-tree .age-recipients differs from origin's:\n%s\nvs\n%s", fresh, n, work, got)
			}
		}
	}
}

// unlabel strips every "# ..." label from the committed .age-recipients of the
// repo (and re-pins), as a repo made before labels existed would look.
func unlabel(t *testing.T, c config.Config) {
	t.Helper()
	r := gitwt.New(c.SyncDir)
	p := filepath.Join(c.SyncDir, ".age-recipients")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "age1") {
			l = strings.Fields(l)[0]
		}
		out = append(out, l)
	}
	if err := os.WriteFile(p, []byte(strings.Join(out, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := agecrypt.PinCurrent(r); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run("add", ".age-recipients"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run("diff", "--cached", "--quiet"); err == nil {
		return // nothing carried a label
	}
	if _, err := r.Run("-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "unlabel"); err != nil {
		t.Fatal(err)
	}
	if err := r.Push("main"); err != nil {
		t.Fatal(err)
	}
}

// Even when this clone has merged the other host's commit (so HEAD contains
// it), that commit is not the migration: its recipients are not the migrated set.
func TestLegacyRecord_AClassicCommitThatHeadContainsIsStillNotTheMigration(t *testing.T) {
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
	if _, err := c.repo.Run("fetch", "-q", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.repo.Run("-c", "user.name=t", "-c", "user.email=t@example.com", "merge", "-q", "--no-edit", "origin/main"); err != nil {
		t.Fatal(err)
	}
	makeLegacyRecord(t, c.cfg.SyncDir)
	code, out, errb := rekey(t, c, args...)
	if code != 1 || !strings.Contains(out+errb, "cannot tell which commit was the migration") {
		t.Fatalf("exit %d:\n%s%s", code, out, errb)
	}
	if _, blocked := agecrypt.RekeyBlocksSync(c.repo); !blocked {
		t.Error("the daemon was unblocked")
	}
}
