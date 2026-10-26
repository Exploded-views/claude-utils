package main

import (
	"bytes"
	"encoding/json"
	"github.com/MarimerLLC/claude-utils/internal/config"
	"os"
	"path/filepath"
	"sort"
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

// N4: messages that matched the wrong situation.
func TestRekeyPQ_DryRunOnAHalfMigratedRepoSaysHowToFinishOrUndo(t *testing.T) {
	for _, stage := range []string{"migrated, not pushed", "scheme not yet repointed"} {
		t.Run(stage, func(t *testing.T) {
			c, newRec, _ := unpublishedRekey(t)
			if stage == "scheme not yet repointed" {
				if _, err := c.repo.Run("config", "--unset", agecrypt.SchemeConfigKey); err != nil {
					t.Fatal(err)
				}
			}
			code, out, errb := rekey(t, c, "--recovery-recipient", newRec, "--host", "vm-1", "--fresh-root")
			all := out + errb
			if code != 0 {
				t.Errorf("exit %d, want 0:\n%s", code, all)
			}
			if strings.Contains(all, "--drop-backup") {
				t.Errorf("a half-migrated repo must not be told to drop its backup:\n%s", all)
			}
			for _, want := range []string{"interrupted", "--apply", "--rollback"} {
				if !strings.Contains(all, want) {
					t.Errorf("dry run lacks %q:\n%s", want, all)
				}
			}
		})
	}
}

func TestRekeyPQ_DryRunOnACompletedRepoSaysAlreadyDone(t *testing.T) {
	c := newClassicRepo(t)
	_, newRec := newPQKey(t, "newrec")
	if code, o, e := rekey(t, c, "--recovery-recipient", newRec, "--host", "vm-1", "--fresh-root", "--apply"); code != 0 {
		t.Fatalf("%s%s", o, e)
	}
	code, out, errb := rekey(t, c, "--recovery-recipient", newRec, "--host", "vm-1", "--fresh-root")
	if code != 0 || !strings.Contains(out+errb, "already done") {
		t.Errorf("exit %d, want 0 and \"already done\":\n%s%s", code, out, errb)
	}
}

func TestRekeyPQ_CompletedRerunWithOtherFlagsIsNotCalledInterrupted(t *testing.T) {
	c := newClassicRepo(t)
	_, newRec := newPQKey(t, "newrec")
	_, other := newPQKey(t, "other")
	if code, o, e := rekey(t, c, "--recovery-recipient", newRec, "--host", "vm-1", "--fresh-root", "--apply"); code != 0 {
		t.Fatalf("%s%s", o, e)
	}
	code, out, errb := rekey(t, c, "--recovery-recipient", other, "--host", "vm-1", "--apply")
	all := out + errb
	if strings.Contains(all, "interrupted") || strings.Contains(all, "public recipient") {
		t.Errorf("a finished rekey is neither interrupted nor migrated again:\n%s", all)
	}
	if code != 0 || !strings.Contains(all, "already done") {
		t.Errorf("exit %d, want 0 and \"already done\":\n%s", code, all)
	}
}

func TestRekeyPQ_AllRollbackSkipsPublishedRepos(t *testing.T) {
	cfgs := rekeyHost(t, "a", "b")
	_, rec := newPQKey(t, "rec")
	if code, o, e := runRcpt("rekey-pq", "--all", "--recovery-recipient", rec, "--host", "vm-1", "--fresh-root", "--apply"); code != 0 {
		t.Fatalf("%s%s", o, e)
	}
	code, out, errb := runRcpt("rekey-pq", "--all", "--rollback")
	if code != 0 {
		t.Fatalf("--all --rollback on published repos: exit %d\n%s%s", code, out, errb)
	}
	if strings.Count(out, "published; nothing to roll back") != 2 {
		t.Errorf("each published repo must say so:\n%s%s", out, errb)
	}
	for n, c := range cfgs {
		if agecrypt.SchemeOf(gitwt.New(c.SyncDir)) != agecrypt.SchemePQ {
			t.Errorf("%s: a published repo was touched", n)
		}
	}
}

// N5: a corrupt restore record is not "missing"; the advice must not be to
// drop the backup, which is all that is left of the classic history.
func TestRekeyPQ_CorruptRecordGivesRecoveryAdviceNotDropBackup(t *testing.T) {
	c, newRec, _ := unpublishedRekey(t)
	rec := filepath.Join(c.cfg.SyncDir, ".git", "memsync-pre-pq.json")
	if err := os.WriteFile(rec, []byte(`{"tip": "abc`), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"apply":    {"--recovery-recipient", newRec, "--host", "vm-1", "--fresh-root", "--apply"},
		"dry run":  {"--recovery-recipient", newRec, "--host", "vm-1", "--fresh-root"},
		"rollback": {"--rollback"},
	} {
		code, out, errb := rekey(t, c, args...)
		all := out + errb
		if code == 0 {
			t.Errorf("%s: a corrupt record must fail:\n%s", name, all)
		}
		if strings.Contains(all, "--drop-backup") {
			t.Errorf("%s: must not advise --drop-backup:\n%s", name, all)
		}
		for _, want := range []string{"unreadable", agecrypt.BackupRef} {
			if !strings.Contains(all, want) {
				t.Errorf("%s: advice lacks %q:\n%s", name, want, all)
			}
		}
	}
	if _, err := c.repo.Run("rev-parse", "--verify", "--quiet", agecrypt.BackupRef); err != nil {
		t.Error("the backup ref is gone")
	}
}

func TestRekeyPQ_RecordWriteLeavesNoTempFile(t *testing.T) {
	c, _, _ := unpublishedRekey(t)
	ents, _ := os.ReadDir(filepath.Join(c.cfg.SyncDir, ".git"))
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "memsync-pre-pq.json") && e.Name() != "memsync-pre-pq.json" {
			t.Errorf("leftover %s", e.Name())
		}
	}
}

// N6: HEAD is read under the sync lock. A cycle that moves HEAD (and records
// it as synced) just before the lock is taken must not leave the synced-head
// record on a commit the rollback then discards.
func TestRekeyPQ_RollbackReadsHeadUnderTheSyncLock(t *testing.T) {
	c, _, oldTip := unpublishedRekey(t)
	rec := filepath.Join(c.cfg.SyncDir, ".state", "synced-head")
	rollbackBeforeLock = func() {
		commitEmpty(t, c.repo, "a daemon cycle just before the lock")
		h, _ := c.repo.Run("rev-parse", "HEAD")
		if err := os.MkdirAll(filepath.Dir(rec), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(rec, []byte(strings.TrimSpace(h)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { rollbackBeforeLock = nil })
	if code, o, e := rollbackCmd(t, c, "--discard-local-commits"); code != 0 {
		t.Fatalf("rollback: %d\n%s%s", code, o, e)
	}
	got, _ := os.ReadFile(rec)
	if strings.TrimSpace(string(got)) != oldTip {
		t.Errorf("synced-head record %q, want the restored tip %q", strings.TrimSpace(string(got)), oldTip)
	}
}

// N8: `status` says the daemon is not syncing a repo with an unfinished rekey.
func TestStatus_SaysSyncingIsStoppedForAnUnfinishedRekey(t *testing.T) {
	c, newRec, _ := unpublishedRekey(t)
	has := func() bool {
		for _, l := range encryptionLines(c.cfg) {
			if strings.Contains(l, "rekey-pq") && strings.Contains(l, "not syncing") {
				return true
			}
		}
		return false
	}
	if !has() {
		t.Errorf("status lacks the rekey-pq line: %q", encryptionLines(c.cfg))
	}
	unblockPush(t, c.origin)
	if code, o, e := rekey(t, c, "--recovery-recipient", newRec, "--host", "vm-1", "--fresh-root", "--apply"); code != 0 {
		t.Fatalf("%s%s", o, e)
	}
	if has() {
		t.Errorf("status still says syncing is stopped after the rekey finished: %q", encryptionLines(c.cfg))
	}
}

// N3: an unfinished park or an interrupted pull must be recovered by the
// daemon before the history is rewritten under it.
func TestRekeyPQ_ApplyRefusesWhileAParkOrPullIsUnfinished(t *testing.T) {
	for _, name := range []string{"parked.json", "pull-running"} {
		t.Run(name, func(t *testing.T) {
			c := newClassicRepo(t)
			_, newRec := newPQKey(t, "newrec")
			st := filepath.Join(c.cfg.SyncDir, ".state")
			if err := os.MkdirAll(st, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(st, name), []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			before := c.state(t)
			code, out, errb := rekey(t, c, "--recovery-recipient", newRec, "--host", "vm-1", "--fresh-root", "--apply")
			if code == 0 {
				t.Fatalf("apply went ahead with %s present\n%s%s", name, out, errb)
			}
			for _, want := range []string{name, "daemon", "rerun"} {
				if !strings.Contains(errb, want) {
					t.Errorf("refusal lacks %q:\n%s", want, errb)
				}
			}
			if after := c.state(t); after != before {
				t.Error("a refused apply changed the repo")
			}
		})
	}
}

// legacyKeys are exactly the keys the restore records of the 9c26984 build have.
var legacyKeys = []string{"classicIdentity", "config", "freshRoot", "label", "newIdentity", "newRecipient", "priorScheme", "priorSchemeSet", "recovery", "tip"}

// makeLegacyRecord rewrites the restore record of the repo at syncDir into the
// shape an older build wrote: no stage and no newCommit.
func makeLegacyRecord(t *testing.T, syncDir string) {
	t.Helper()
	p := filepath.Join(syncDir, ".git", "memsync-pre-pq.json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "stage")
	delete(m, "newCommit")
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != strings.Join(legacyKeys, ",") {
		t.Fatalf("legacy record keys %v, want %v", keys, legacyKeys)
	}
	out, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(p, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func recordMap(t *testing.T, syncDir string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(syncDir, ".git", "memsync-pre-pq.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// B1: the live records are legacy ones. A rerun must finish them as done.
func TestLegacyRecord_RerunIsAlreadyDone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fresh bool
		push  bool // the extra commit is also on origin
	}{
		{"fresh root, origin moved on", true, true},
		{"fresh root, HEAD ahead of origin", true, false},
		{"on top of history, origin moved on", false, true},
		{"on top of history, HEAD ahead of origin", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newClassicRepo(t)
			_, newRec := newPQKey(t, "newrec")
			args := []string{"--recovery-recipient", newRec, "--host", "vm-1", "--apply"}
			if tc.fresh {
				args = append(args, "--fresh-root")
			}
			if code, o, e := rekey(t, c, args...); code != 0 {
				t.Fatalf("%s%s", o, e)
			}
			commitEmpty(t, c.repo, "auto: daemon sync")
			if tc.push {
				if err := c.repo.Push("main"); err != nil {
					t.Fatal(err)
				}
			}
			makeLegacyRecord(t, c.cfg.SyncDir)
			cfgBefore, _ := os.ReadFile(c.cfg.Path())
			code, out, errb := rekey(t, c, args...)
			if code != 0 || !strings.Contains(out, "already done") {
				t.Fatalf("exit %d, want 0 and \"already done\":\n%s%s", code, out, errb)
			}
			if cfgAfter, _ := os.ReadFile(c.cfg.Path()); !bytes.Equal(cfgAfter, cfgBefore) {
				t.Errorf("config.json was rewritten:\n%s\nvs\n%s", cfgAfter, cfgBefore)
			}
			m := recordMap(t, c.cfg.SyncDir)
			if m["stage"] != "published" || m["newCommit"] == nil || m["newCommit"] == "" {
				t.Errorf("record not completed: stage %v newCommit %v", m["stage"], m["newCommit"])
			}
		})
	}
}

func TestLegacyRecord_RerunWithOtherFlagsIsNotInterrupted(t *testing.T) {
	c := newClassicRepo(t)
	_, newRec := newPQKey(t, "newrec")
	_, other := newPQKey(t, "other")
	if code, o, e := rekey(t, c, "--recovery-recipient", newRec, "--host", "vm-1", "--fresh-root", "--apply"); code != 0 {
		t.Fatalf("%s%s", o, e)
	}
	commitEmpty(t, c.repo, "auto: daemon sync")
	if err := c.repo.Push("main"); err != nil {
		t.Fatal(err)
	}
	makeLegacyRecord(t, c.cfg.SyncDir)
	code, out, errb := rekey(t, c, "--recovery-recipient", other, "--host", "vm-1", "--apply")
	all := out + errb
	if code != 0 || !strings.Contains(all, "already done") || strings.Contains(all, "interrupted") {
		t.Fatalf("exit %d:\n%s", code, all)
	}
	if cc, _ := config.Load(c.cfg.Path()); len(cc.RecoveryRecipients) != 1 || cc.RecoveryRecipients[0] != newRec {
		t.Errorf("recoveryRecipients rewritten: %v", cc.RecoveryRecipients)
	}
}

// setRecordStage rewrites the stage of the repo's restore record.
func setRecordStage(t *testing.T, syncDir, stage string) {
	t.Helper()
	m := recordMap(t, syncDir)
	m["stage"] = stage
	out, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(syncDir, ".git", "memsync-pre-pq.json"), out, 0o600); err != nil {
		t.Fatal(err)
	}
}

// S1: pushed, then a crash before config.json and the stage were saved, and
// the daemon is blocked on the unfinished stage. The rerun must finish the tail.
func TestRekeyPQ_SyncedButUnfinishedStageRunsThePublishTail(t *testing.T) {
	c := newClassicRepo(t)
	_, newRec := newPQKey(t, "newrec")
	cfgBefore, _ := os.ReadFile(c.cfg.Path())
	args := []string{"--recovery-recipient", newRec, "--host", "vm-1", "--fresh-root", "--apply"}
	if code, o, e := rekey(t, c, args...); code != 0 {
		t.Fatalf("%s%s", o, e)
	}
	commitEmpty(t, c.repo, "auto: daemon sync")
	if err := c.repo.Push("main"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.cfg.Path(), cfgBefore, 0o600); err != nil {
		t.Fatal(err)
	}
	setRecordStage(t, c.cfg.SyncDir, "migrated")
	if _, blocked := agecrypt.RekeyBlocksSync(c.repo); !blocked {
		t.Fatal("setup: the daemon must be blocked")
	}
	code, out, errb := rekey(t, c, args...)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errb)
	}
	if cc, _ := config.Load(c.cfg.Path()); cc.EncryptionScheme != "pq" || cc.HostLabel != "vm-1" {
		t.Errorf("config.json not saved: %+v", cc)
	}
	if why, blocked := agecrypt.RekeyBlocksSync(c.repo); blocked {
		t.Errorf("the daemon stays blocked: %s", why)
	}
}

func dropBackup(t *testing.T, c classicRepo) (int, string) {
	t.Helper()
	var o, e bytes.Buffer
	code := recipientsCmd(&o, &e, []string{"rekey-pq", "--config", c.cfg.Path(), "--drop-backup"})
	return code, o.String() + e.String()
}

// S3: the backup is the rollback point; it goes only once the rekey is published.
func TestRekeyPQ_DropBackupRefusesAnUnpublishedRekey(t *testing.T) {
	c, _, _ := unpublishedRekey(t)
	code, out := dropBackup(t, c)
	if code == 0 {
		t.Fatalf("drop-backup succeeded on an unpublished rekey\n%s", out)
	}
	if _, err := c.repo.Run("rev-parse", "--verify", "--quiet", agecrypt.BackupRef); err != nil {
		t.Error("the backup ref was dropped")
	}
	if _, blocked := agecrypt.RekeyBlocksSync(c.repo); !blocked {
		t.Error("the daemon was unblocked")
	}
	// the same for a legacy record whose push never happened
	makeLegacyRecord(t, c.cfg.SyncDir)
	if code, out := dropBackup(t, c); code == 0 {
		t.Fatalf("drop-backup succeeded on an unpublished legacy rekey\n%s", out)
	}
}

func TestRekeyPQ_DropBackupAllowsAPublishedRekeyLegacyOrNot(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		c := newClassicRepo(t)
		_, newRec := newPQKey(t, "newrec")
		if code, o, e := rekey(t, c, "--recovery-recipient", newRec, "--host", "vm-1", "--fresh-root", "--apply"); code != 0 {
			t.Fatalf("%s%s", o, e)
		}
		commitEmpty(t, c.repo, "auto: daemon sync")
		if err := c.repo.Push("main"); err != nil {
			t.Fatal(err)
		}
		if legacy {
			makeLegacyRecord(t, c.cfg.SyncDir)
		}
		if code, out := dropBackup(t, c); code != 0 {
			t.Errorf("legacy=%v: published rekey refused: %s", legacy, out)
		}
	}
}

// crashedBeforeBackupRef leaves what a crash between writing the restore
// record and creating the backup ref leaves: a "started" record, no ref, HEAD
// still on the classic tip.
func crashedBeforeBackupRef(t *testing.T) (classicRepo, string) {
	t.Helper()
	c, newRec, oldTip := unpublishedRekey(t)
	if _, err := c.repo.Run("reset", "--hard", "-q", oldTip); err != nil {
		t.Fatal(err)
	}
	if _, err := c.repo.Run("update-ref", "-d", agecrypt.BackupRef); err != nil {
		t.Fatal(err)
	}
	if err := c.repo.ConfigSet(agecrypt.IdentityConfigKey, filepath.ToSlash(c.idPath)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.repo.Run("config", "--unset", agecrypt.SchemeConfigKey); err != nil {
		t.Fatal(err)
	}
	if err := agecrypt.PinCurrent(c.repo); err != nil { // the classic pin, as before the apply
		t.Fatal(err)
	}
	setRecordStage(t, c.cfg.SyncDir, "started")
	return c, newRec
}

func TestRekeyPQ_RecordWithoutBackupRefIsNotADeadEnd(t *testing.T) {
	c, newRec := crashedBeforeBackupRef(t)
	unblockPush(t, c.origin)
	if code, o, e := rekey(t, c, "--recovery-recipient", newRec, "--host", "vm-1", "--fresh-root", "--apply"); code != 0 {
		t.Fatalf("apply after the crash: %d\n%s%s", code, o, e)
	}
	if agecrypt.SchemeOf(c.repo) != agecrypt.SchemePQ {
		t.Error("not migrated")
	}
}

func TestRekeyPQ_RollbackOfARecordWithoutBackupRefClearsIt(t *testing.T) {
	c, _ := crashedBeforeBackupRef(t)
	if code, o, e := rollbackCmd(t, c); code != 0 {
		t.Fatalf("rollback: %d\n%s%s", code, o, e)
	}
	if _, err := os.Stat(filepath.Join(c.cfg.SyncDir, ".git", "memsync-pre-pq.json")); err == nil {
		t.Error("the record is still there")
	}
}

func TestRekeyPQ_FailedBackupRefLeavesNoRecord(t *testing.T) {
	c := newClassicRepo(t)
	_, newRec := newPQKey(t, "newrec")
	// a file where the ref's directory should be makes update-ref fail
	if err := os.WriteFile(filepath.Join(c.cfg.SyncDir, ".git", "refs", "memsync"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := rekey(t, c, "--recovery-recipient", newRec, "--host", "vm-1", "--fresh-root", "--apply"); code == 0 {
		t.Fatal("apply must fail")
	}
	if _, err := os.Stat(filepath.Join(c.cfg.SyncDir, ".git", "memsync-pre-pq.json")); err == nil {
		t.Error("a failed apply left a restore record")
	}
}

// S5: with a legacy record the migrated commit is inferred, so the commits
// made since are still listed, and --discard-local-commits still lists first.
func TestRekeyPQ_LegacyRollbackListsTheCommitsItWouldDrop(t *testing.T) {
	c, _, oldTip := unpublishedRekey(t)
	makeLegacyRecord(t, c.cfg.SyncDir)
	// the migrated commit alone: nothing to discard
	commitEmpty(t, c.repo, "late legacy note")
	code, out, errb := rollbackCmd(t, c)
	all := out + errb
	if code == 0 || !strings.Contains(all, "late legacy note") || !strings.Contains(all, "1 commit") || strings.Contains(all, "older build") {
		t.Fatalf("exit %d, want a refusal listing the late commit:\n%s", code, all)
	}
	if code, o, e := rollbackCmd(t, c, "--discard-local-commits"); code != 0 {
		t.Fatalf("discard: %d\n%s%s", code, o, e)
	}
	if h, _ := c.repo.Run("rev-parse", "HEAD"); strings.TrimSpace(h) != oldTip {
		t.Error("HEAD not back at the old tip")
	}
}

func TestRekeyPQ_LegacyRollbackWithoutExtraCommitsNeedsNoFlag(t *testing.T) {
	c, _, oldTip := unpublishedRekey(t)
	makeLegacyRecord(t, c.cfg.SyncDir)
	if code, o, e := rollbackCmd(t, c); code != 0 {
		t.Fatalf("rollback: %d\n%s%s", code, o, e)
	}
	if h, _ := c.repo.Run("rev-parse", "HEAD"); strings.TrimSpace(h) != oldTip {
		t.Error("HEAD not back at the old tip")
	}
}
