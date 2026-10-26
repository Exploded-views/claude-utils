package main

import (
	"encoding/json"
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

// A crash after the push but before the record said "published": the commit
// the record names is an ancestor of origin/main, which is as good as done.
func TestRekeyPQ_NewCommitAncestorOfOriginIsDone(t *testing.T) {
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
	rec := filepath.Join(c.cfg.SyncDir, ".git", "memsync-pre-pq.json")
	b, err := os.ReadFile(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rec, []byte(strings.Replace(string(b), `"stage": "published"`, `"stage": "migrated"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errb := rekey(t, c, args...)
	if code != 0 || !strings.Contains(out, "already done") {
		t.Fatalf("exit %d, want \"already done\":\n%s%s", code, out, errb)
	}
}

// unpublishedRekey applies a rekey whose push is blocked, so it is local only.
func unpublishedRekey(t *testing.T) (classicRepo, string, string) {
	t.Helper()
	c := newClassicRepo(t)
	_, newRec := newPQKey(t, "newrec")
	oldTip, _ := c.repo.Run("rev-parse", "HEAD")
	blockPush(t, c.origin)
	if code, o, e := rekey(t, c, "--recovery-recipient", newRec, "--host", "vm-1", "--fresh-root", "--apply"); code == 0 {
		t.Fatalf("push was blocked yet exit 0\n%s%s", o, e)
	}
	return c, newRec, strings.TrimSpace(oldTip)
}

func rollbackCmd(t *testing.T, c classicRepo, extra ...string) (int, string, string) {
	t.Helper()
	return rekey(t, c, append([]string{"--rollback"}, extra...)...)
}

// S2: local commits made after the rekey are not silently dropped.
func TestRekeyPQ_RollbackRefusesToDropLocalCommits(t *testing.T) {
	c, _, oldTip := unpublishedRekey(t)
	head, _ := c.repo.Run("rev-parse", "HEAD")
	commitEmpty(t, c.repo, "note written after the rekey")
	commitEmpty(t, c.repo, "second late note")
	code, out, errb := rollbackCmd(t, c)
	if code == 0 {
		t.Fatalf("rollback dropped local commits silently\n%s%s", out, errb)
	}
	for _, want := range []string{"2 commits", "note written after the rekey", "second late note", "--discard-local-commits"} {
		if !strings.Contains(errb, want) {
			t.Errorf("refusal lacks %q:\n%s", want, errb)
		}
	}
	if agecrypt.SchemeOf(c.repo) != agecrypt.SchemePQ {
		t.Error("a refused rollback changed the repo")
	}
	if h, _ := c.repo.Run("rev-parse", "HEAD"); h == head {
		t.Fatal("setup: the late commits are missing")
	}
	if code, out, errb := rollbackCmd(t, c, "--discard-local-commits"); code != 0 {
		t.Fatalf("rollback with --discard-local-commits: %d\n%s%s", code, out, errb)
	}
	if h, _ := c.repo.Run("rev-parse", "HEAD"); strings.TrimSpace(h) != oldTip {
		t.Error("HEAD not back at the old tip")
	}
}

func TestRekeyPQ_DiscardFlagGoesOnlyWithRollback(t *testing.T) {
	c := newClassicRepo(t)
	_, newRec := newPQKey(t, "newrec")
	if code, _, _ := rekey(t, c, "--recovery-recipient", newRec, "--host", "vm-1", "--discard-local-commits"); code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
}

// S3: a rollback needs the classic identity; it is renamed back if retired.
func TestRekeyPQ_RollbackRestoresARetiredClassicIdentity(t *testing.T) {
	c, _, oldTip := unpublishedRekey(t)
	if err := os.Rename(c.idPath, c.idPath+".classic-retired"); err != nil {
		t.Fatal(err)
	}
	if code, o, e := rollbackCmd(t, c); code != 0 {
		t.Fatalf("rollback: %d\n%s%s", code, o, e)
	}
	if _, err := os.Stat(c.idPath); err != nil {
		t.Errorf("the classic identity was not put back: %v", err)
	}
	if h, _ := c.repo.Run("rev-parse", "HEAD"); strings.TrimSpace(h) != oldTip {
		t.Error("HEAD not back at the old tip")
	}
	if err := agecrypt.CheckKeyed(c.repo); err != nil {
		t.Errorf("classic repo does not decrypt: %v", err)
	}
}

func TestRekeyPQ_RollbackRefusesWithoutAClassicIdentity(t *testing.T) {
	c, _, _ := unpublishedRekey(t)
	if err := os.Remove(c.idPath); err != nil {
		t.Fatal(err)
	}
	code, out, errb := rollbackCmd(t, c)
	if code == 0 {
		t.Fatalf("rollback reported success without the classic identity\n%s%s", out, errb)
	}
	if !strings.Contains(errb, "classic identity") || !strings.Contains(errb, filepath.Base(c.idPath)) {
		t.Errorf("the refusal must name the missing classic identity:\n%s", errb)
	}
	if agecrypt.SchemeOf(c.repo) != agecrypt.SchemePQ {
		t.Error("a refused rollback changed the repo")
	}
}

// S3: a clone whose restore record names the classic identity still needs it
// for its rollback, so retirement keeps the file.
func TestRekeyPQ_KeepsTheClassicIdentityNamedByAnotherClonesRecord(t *testing.T) {
	c := newClassicRepo(t)
	_, newRec := newPQKey(t, "newrec")
	sib := c.cfg.SyncDir + ".other"
	sr := gitwt.New(sib)
	if err := sr.Init("main"); err != nil {
		t.Fatal(err)
	}
	rec, _ := json.Marshal(agecrypt.PreRekeyState{ClassicIdentity: c.idPath, Stage: "migrated"})
	if err := os.WriteFile(filepath.Join(sib, ".git", agecrypt.RekeyStateFile), rec, 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errb := rekey(t, c, "--recovery-recipient", newRec, "--host", "vm-1", "--fresh-root", "--apply")
	if code != 0 {
		t.Fatalf("%d %s%s", code, out, errb)
	}
	if _, err := os.Stat(c.idPath); err != nil {
		t.Error("the classic identity was retired although another clone's restore record names it")
	}
	if !strings.Contains(out, ".other") {
		t.Errorf("the hint must name the clone:\n%s", out)
	}
}

// N1: a crash between SetScheme and PinCurrent leaves the pin stale. Every
// resume stage repoints again, so the push guard does not refuse for ever.
func TestRekeyPQ_ResumeRepinsAfterACrashBetweenSchemeAndPin(t *testing.T) {
	c, newRec, _ := unpublishedRekey(t)
	pin := filepath.Join(c.cfg.SyncDir, ".git", "memage-pinned")
	if err := os.WriteFile(pin, []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if u, _ := agecrypt.Unreviewed(c.repo); !u {
		t.Fatal("setup: the pin must be stale")
	}
	unblockPush(t, c.origin)
	code, out, errb := rekey(t, c, "--recovery-recipient", newRec, "--host", "vm-1", "--fresh-root", "--apply")
	if code != 0 {
		t.Fatalf("resume: exit %d\n%s%s", code, out, errb)
	}
	if u, _ := agecrypt.Unreviewed(c.repo); u {
		t.Error("the pin is still stale after the resume")
	}
	if originTip(t, c.origin) == "" {
		t.Error("nothing pushed")
	}
	if n, _ := gitwt.New(c.origin).Run("rev-list", "--count", "main"); strings.TrimSpace(n) != "1" {
		t.Errorf("origin has %s commits", n)
	}
}

// N2: a crash between `update-ref HEAD` and the index update leaves the old
// classic blob ids in the index. The resume resets the index to HEAD.
func TestRekeyPQ_ResumeResetsAnIndexLeftOnClassicBlobs(t *testing.T) {
	for _, stage := range []string{"migrated, not pushed", "scheme not yet repointed"} {
		t.Run(stage, func(t *testing.T) {
			c, newRec, _ := unpublishedRekey(t)
			if stage == "scheme not yet repointed" {
				if err := c.repo.ConfigSet(agecrypt.IdentityConfigKey, filepath.ToSlash(c.idPath)); err != nil {
					t.Fatal(err)
				}
				if _, err := c.repo.Run("config", "--unset", agecrypt.SchemeConfigKey); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := c.repo.Run("read-tree", agecrypt.BackupRef); err != nil {
				t.Fatal(err)
			}
			if d, _ := c.repo.Run("diff-index", "--cached", "--name-only", "HEAD"); strings.TrimSpace(d) == "" {
				t.Fatal("setup: the index must differ from HEAD")
			}
			unblockPush(t, c.origin)
			if code, o, e := rekey(t, c, "--recovery-recipient", newRec, "--host", "vm-1", "--fresh-root", "--apply"); code != 0 {
				t.Fatalf("resume: exit %d\n%s%s", code, o, e)
			}
			if d, _ := c.repo.Run("diff-index", "--cached", "--name-only", "HEAD"); strings.TrimSpace(d) != "" {
				t.Errorf("the index still differs from HEAD after the resume:\n%s", d)
			}
			if d, _ := c.repo.Run("status", "--porcelain"); strings.TrimSpace(d) != "" {
				t.Errorf("the work-tree is dirty after the resume:\n%s", d)
			}
		})
	}
}
