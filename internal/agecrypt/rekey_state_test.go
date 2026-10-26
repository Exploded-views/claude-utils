package agecrypt

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rekeyFx is a pushed classic repo with one note, ready for a rekey to pq.
type rekeyFx struct {
	h    *host
	opts RekeyOpts
	rec  string
}

func newRekeyFx(t *testing.T) *rekeyFx {
	t.Helper()
	gitEnv(t)
	remote := bareRemote(t)
	_, rec := newIdentity(t, "rec")
	h := newHost(t, remote, true, rec+" # recovery")
	h.write(t, "projects/p1/memory/a.md", "one\n")
	h.commit(t, "seed")
	if err := h.repo.Push("main"); err != nil {
		t.Fatal(err)
	}
	_, pqRec := newPQIdentity(t, "pqrec")
	return &rekeyFx{h: h, rec: rec, opts: RekeyOpts{
		Label: "vm-1", Recovery: pqRec, ConfiguredRecovery: []string{rec},
		NewIdentityPath: filepath.Join(filepath.Dir(h.idPath), "id-pq"),
	}}
}

// apply migrates locally and returns the plan.
func (f *rekeyFx) apply(t *testing.T) RekeyPlan {
	t.Helper()
	p, err := ApplyRekeyPQ(f.h.repo, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *rekeyFx) emptyCommit(t *testing.T, subject string) {
	t.Helper()
	if _, err := f.h.repo.Run("-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", subject); err != nil {
		t.Fatal(err)
	}
}

func (f *rekeyFx) stage(t *testing.T, s string) {
	t.Helper()
	if err := UpdateRekeyState(f.h.repo, func(st *PreRekeyState) { st.Stage = s }); err != nil {
		t.Fatal(err)
	}
}

func TestRekeyState_ApplyRecordsStagesAndDetectClassifiesThem(t *testing.T) {
	f := newRekeyFx(t)
	repo := f.h.repo
	p := f.apply(t)
	st, err := ReadRekeyState(repo)
	if err != nil {
		t.Fatal(err)
	}
	if st.NewCommit != p.NewCommit || st.NewCommit == "" || st.Stage != RekeyRecMigrated || st.Tip != p.Tip {
		t.Fatalf("record %+v, plan %+v", st, p)
	}
	if fi, _ := os.Stat(filepath.Join(repo.Dir, ".git", RekeyStateFile)); fi == nil || fi.Mode().Perm() != 0o600 && os.PathSeparator == '/' {
		t.Errorf("record mode %v", fi)
	}
	if _, err := os.Stat(filepath.Join(repo.Dir, ".git", RekeyStateFile+".tmp")); err == nil {
		t.Error("temp file left behind")
	}
	// Remote still at the old tip.
	if stage, _, err := DetectRekey(repo, f.opts); err != nil || stage != StageRetryPush {
		t.Fatalf("unpushed: %v %v", stage, err)
	}
	// Other flags on an unfinished record: interrupted.
	o2 := f.opts
	o2.FreshRoot = true
	if _, _, err := DetectRekey(repo, o2); err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Errorf("flags differ: %v", err)
	}
	if err := repo.Push("main"); err != nil {
		t.Fatal(err)
	}
	if stage, _, err := DetectRekey(repo, f.opts); err != nil || stage != StagePublished {
		t.Fatalf("pushed: %v %v", stage, err)
	}
	// The daemon syncs on top: NewCommit is an ancestor of origin/main.
	f.emptyCommit(t, "auto: sync")
	if err := repo.Push("main"); err != nil {
		t.Fatal(err)
	}
	if stage, _, err := DetectRekey(repo, f.opts); err != nil || stage != StageSynced {
		t.Fatalf("synced by ancestry: %v %v", stage, err)
	}
	// A finished record needs no remote and ignores the flags.
	f.stage(t, RekeyRecPublished)
	if stage, _, err := DetectRekey(repo, o2); err != nil || stage != StageSynced {
		t.Fatalf("finished record: %v %v", stage, err)
	}
}

func TestRekeyState_DetectRefusesWhatItCannotClassify(t *testing.T) {
	f := newRekeyFx(t)
	repo := f.h.repo
	if stage, _, err := DetectRekey(repo, f.opts); err != nil || stage != StageFresh {
		t.Fatalf("nothing done: %v %v", stage, err)
	}
	f.apply(t)
	// someone else pushed: neither the backup tip nor the migrated tip
	other := newHostClone(t, repo)
	other.write(t, "projects/p1/memory/z.md", "z\n")
	other.commit(t, "elsewhere")
	if _, err := other.repo.Run("push", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := DetectRekey(repo, f.opts); err == nil || !strings.Contains(err.Error(), "neither") {
		t.Errorf("origin moved: %v", err)
	}
	// record and backup disagree
	if err := UpdateRekeyState(repo, func(s *PreRekeyState) { s.Tip = "deadbeef" }); err != nil {
		t.Fatal(err)
	}
	if _, _, err := DetectRekey(repo, f.opts); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Errorf("tip mismatch: %v", err)
	}
	// record gone: the old advice (drop the backup) stays for a missing record only
	removeRekeyState(repo)
	if _, _, err := DetectRekey(repo, f.opts); err == nil || !strings.Contains(err.Error(), "--drop-backup") {
		t.Errorf("missing record: %v", err)
	}
	// record unreadable: the other advice
	if err := os.WriteFile(filepath.Join(repo.Dir, ".git", RekeyStateFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := DetectRekey(repo, f.opts)
	if !errors.Is(err, ErrRecordUnreadable) || !strings.Contains(err.Error(), "do NOT drop the backup") {
		t.Errorf("unreadable record: %v", err)
	}
}

// newHostClone is a second clone of the same origin, for pushes behind the back.
func newHostClone(t *testing.T, of interface {
	Run(...string) (string, error)
}) *host {
	t.Helper()
	remote, err := of.Run("remote", "get-url", "origin")
	if err != nil {
		t.Fatal(err)
	}
	return newHost(t, strings.TrimSpace(remote), false)
}

func TestRekeyState_ResumeRepairsTheLocalStateIdempotently(t *testing.T) {
	f := newRekeyFx(t)
	repo := f.h.repo
	p := f.apply(t)
	st, _ := ReadRekeyState(repo)
	// the crash: pin stale, index on the classic blobs, scheme gone
	if err := os.WriteFile(filepath.Join(repo.Dir, ".git", pinFile), []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Run("read-tree", BackupRef); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Run("config", "--unset", SchemeConfigKey); err != nil {
		t.Fatal(err)
	}
	if err := checkMigratedRecipients(repo, st); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // idempotent
		plan, err := FinishRekeyLocal(repo, st)
		if err != nil || plan.NewCommit != p.NewCommit {
			t.Fatalf("finish: %+v %v", plan, err)
		}
	}
	if SchemeOf(repo) != SchemePQ {
		t.Error("scheme not repointed")
	}
	if u, _ := Unreviewed(repo); u {
		t.Error("pin stale")
	}
	if out, _ := repo.Run("diff-index", "--cached", "--name-only", "HEAD"); strings.TrimSpace(out) != "" {
		t.Errorf("index differs from HEAD: %s", out)
	}
	if PlanFromState(repo, st).IdentityPath != st.ClassicIdentity {
		t.Error("plan from state")
	}
	// a recipients file that is not the migrated set is refused
	if err := WriteRecipients(repo.Dir, []string{f.rec}); err != nil {
		t.Fatal(err)
	}
	if err := checkMigratedRecipients(repo, st); err == nil {
		t.Error("another recipient set must be refused")
	}
}

func TestRekeyState_RollbackGuards(t *testing.T) {
	f := newRekeyFx(t)
	repo := f.h.repo
	noCfg := func(PriorConfig) error { return nil }
	if _, err := RollbackRekey(repo, RollbackOpts{}, noCfg); err == nil || !strings.Contains(err.Error(), "nothing to roll back") {
		t.Fatalf("no record: %v", err)
	}
	p := f.apply(t)
	f.emptyCommit(t, "late note")
	_, err := RollbackRekey(repo, RollbackOpts{}, noCfg)
	if err == nil || !strings.Contains(err.Error(), "1 commit") || !strings.Contains(err.Error(), "late note") || !strings.Contains(err.Error(), "--discard-local-commits") {
		t.Fatalf("extra commit: %v", err)
	}
	// a legacy record (no NewCommit) cannot tell, so it asks too
	if err := UpdateRekeyState(repo, func(s *PreRekeyState) { s.NewCommit = "" }); err != nil {
		t.Fatal(err)
	}
	if _, err := RollbackRekey(repo, RollbackOpts{}, noCfg); err == nil || !strings.Contains(err.Error(), "does not name the migrated commit") {
		t.Fatalf("legacy record: %v", err)
	}
	if err := UpdateRekeyState(repo, func(s *PreRekeyState) { s.NewCommit = p.NewCommit }); err != nil {
		t.Fatal(err)
	}
	// the classic identity is neither in place nor retired
	if err := os.Rename(f.h.idPath, f.h.idPath+".gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := RollbackRekey(repo, RollbackOpts{DiscardLocalCommits: true}, noCfg); err == nil || !strings.Contains(err.Error(), "classic identity") {
		t.Fatalf("missing identity: %v", err)
	}
	// retired: renamed back
	if err := os.Rename(f.h.idPath+".gone", f.h.idPath+RetiredSuffix); err != nil {
		t.Fatal(err)
	}
	restored := false
	if _, err := RollbackRekey(repo, RollbackOpts{DiscardLocalCommits: true}, func(PriorConfig) error { restored = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if !restored {
		t.Error("config not restored")
	}
	if _, err := os.Stat(f.h.idPath); err != nil {
		t.Errorf("classic identity not put back: %v", err)
	}
	if h, _ := repo.Run("rev-parse", "HEAD"); strings.TrimSpace(h) != p.Tip {
		t.Error("HEAD not at the old tip")
	}
	if _, ok := revOf(repo, BackupRef); ok {
		t.Error("backup ref left")
	}
	if _, err := ReadRekeyState(repo); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("record left: %v", err)
	}
}

func TestRekeyState_RollbackRefusesWhenThingsMoved(t *testing.T) {
	f := newRekeyFx(t)
	repo := f.h.repo
	noCfg := func(PriorConfig) error { return nil }
	f.apply(t)
	if err := os.WriteFile(filepath.Join(repo.Dir, "dirty.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RollbackRekey(repo, RollbackOpts{}, noCfg); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Errorf("dirty tree: %v", err)
	}
	os.Remove(filepath.Join(repo.Dir, "dirty.txt"))
	if err := repo.Push("main"); err != nil {
		t.Fatal(err)
	}
	if _, err := RollbackRekey(repo, RollbackOpts{}, noCfg); err == nil || !strings.Contains(err.Error(), "remote has moved") {
		t.Errorf("published: %v", err)
	}
	if err := UpdateRekeyState(repo, func(s *PreRekeyState) { s.Tip = "x" }); err != nil {
		t.Fatal(err)
	}
	if _, err := RollbackRekey(repo, RollbackOpts{}, noCfg); err == nil {
		t.Error("record and backup disagree")
	}
}

func TestRekeyState_RekeyBlocksSync(t *testing.T) {
	f := newRekeyFx(t)
	repo := f.h.repo
	if why, blocked := RekeyBlocksSync(repo); blocked {
		t.Fatalf("no record blocks: %s", why)
	}
	f.apply(t)
	for _, tc := range []struct {
		stage   string
		blocked bool
	}{
		{RekeyRecStarted, true}, {RekeyRecCommitted, true}, {RekeyRecMigrated, true},
		{RekeyRecPublished, false}, {RekeyRecDone, false}, {"", false},
	} {
		f.stage(t, tc.stage)
		why, blocked := RekeyBlocksSync(repo)
		if blocked != tc.blocked || (blocked && !strings.Contains(why, "rekey-pq")) {
			t.Errorf("stage %q: blocked=%v %q", tc.stage, blocked, why)
		}
	}
	// not pq yet blocks whatever the stage says
	f.stage(t, RekeyRecPublished)
	if _, err := repo.Run("config", "--unset", SchemeConfigKey); err != nil {
		t.Fatal(err)
	}
	if why, blocked := RekeyBlocksSync(repo); !blocked || !strings.Contains(why, "post-quantum") {
		t.Errorf("scheme not pq: %v %q", blocked, why)
	}
	f.stage(t, "")
	if why, blocked := RekeyBlocksSync(repo); !blocked || !strings.Contains(why, "unknown") {
		t.Errorf("legacy record, scheme not pq: %v %q", blocked, why)
	}
	if err := os.WriteFile(filepath.Join(repo.Dir, ".git", RekeyStateFile), []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if why, blocked := RekeyBlocksSync(repo); !blocked || !strings.Contains(why, "unreadable") {
		t.Errorf("unreadable: %v %q", blocked, why)
	}
}

func TestRekeyState_PlanRefusals(t *testing.T) {
	f := newRekeyFx(t)
	repo := f.h.repo
	bad := f.opts
	bad.Label = "Bad Label"
	if _, err := PlanRekeyPQ(repo, bad, false); err == nil || !strings.Contains(err.Error(), "--host") {
		t.Errorf("label: %v", err)
	}
	bad = f.opts
	bad.Recovery = f.rec // classic key
	if _, err := PlanRekeyPQ(repo, bad, false); err == nil {
		t.Error("a classic recovery key must be refused")
	}
	for _, name := range []string{"parked.json", "pull-running"} {
		p := filepath.Join(repo.Dir, ".state", name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := PlanRekeyPQ(repo, f.opts, false); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: %v", name, err)
		}
		os.Remove(p)
	}
	plan, err := PlanRekeyPQ(repo, f.opts, true)
	if err != nil || plan.Blobs == 0 || plan.Commits == 0 {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	f.emptyCommit(t, "unpushed")
	if _, err := PlanRekeyPQ(repo, f.opts, false); err == nil || !strings.Contains(err.Error(), "differs from origin/main") {
		t.Errorf("unpushed: %v", err)
	}
}

func TestRekeyState_VerifyAndDropBackup(t *testing.T) {
	f := newRekeyFx(t)
	repo := f.h.repo
	if err := DropBackup(repo); err == nil {
		t.Error("a classic repo has no backup to drop")
	}
	if _, _, err := VerifyHybridRemote(repo, "main", 0); err == nil {
		t.Error("the classic remote must not verify as hybrid")
	}
	f.apply(t)
	if err := repo.Push("main"); err != nil {
		t.Fatal(err)
	}
	commits, blobs, err := VerifyHybridRemote(repo, "main", 0)
	if err != nil || commits != 2 || blobs == 0 {
		t.Fatalf("verify: %d %d %v", commits, blobs, err)
	}
	if _, _, err := VerifyHybridRemote(repo, "main", 1); err == nil {
		t.Error("a wrong commit count must fail")
	}
	if err := DropBackup(repo); err != nil {
		t.Fatal(err)
	}
	if _, ok := revOf(repo, BackupRef); ok {
		t.Error("backup ref left")
	}
	if _, err := ReadRekeyStateAt(repo.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("record left: %v", err)
	}
	if err := DropBackup(repo); err == nil {
		t.Error("nothing left to drop")
	}
}
