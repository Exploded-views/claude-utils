package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/lock"
)

// ---- B-1: label --all never touches a repo it refuses ----

func TestLabelSelfAll_RekeyBlockedRepoIsRefusedAndUntouched(t *testing.T) {
	cfgs := routedHost(t, []string{"personal", "projects"}, "personal", nil)
	good := encryptRouted(t, cfgs["personal"])
	bad := encryptRouted(t, cfgs["projects"])
	// A garbage restore record blocks the repo (RekeyBlocksSync: unreadable).
	rec := filepath.Join(bad.SyncDir, ".git", agecrypt.RekeyStateFile)
	if err := os.WriteFile(rec, []byte("{garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, blocked := agecrypt.RekeyBlocksSync(gitwt.New(bad.SyncDir)); !blocked {
		t.Fatal("setup: the repo must be blocked")
	}
	headBefore := headRev(t, bad.SyncDir)
	f := &fakeSvc{status: "running"}
	installFakeSvc(t, f)
	var held []*lock.Lock
	take := func() {
		for _, d := range []string{good.SyncDir, bad.SyncDir} {
			l, err := lock.Acquire(d)
			if err != nil {
				t.Fatal(err)
			}
			held = append(held, l)
		}
	}
	drop := func() {
		for _, l := range held {
			l.Release()
		}
		held = nil
	}
	take()
	t.Cleanup(drop)
	f.onStop = drop
	startFn = func() error { f.calls = append(f.calls, "start"); take(); return nil }

	code, out, errs := runRcpt("label", "self", "--host", "vm-1", "--all")
	if code == 0 {
		t.Fatalf("a blocked repo must make --all fail:\n%s%s", out, errs)
	}
	if !strings.Contains(errs, "finish or roll back the rekey first") || !strings.Contains(errs, filepath.Base(bad.SyncDir)) {
		t.Errorf("the blocked repo is not named with the rekey advice:\n%s", errs)
	}
	if headRev(t, bad.SyncDir) != headBefore {
		t.Error("the blocked repo was committed to")
	}
	if l, _ := agecrypt.ListLabels(good.SyncDir); len(l) != 1 || l[0] != "vm-1" {
		t.Errorf("the good repo was not labelled: %v", l)
	}
	if f.did() != "stop,start" {
		t.Errorf("service calls = %q, want one stop and one start", f.did())
	}
}

// ---- S-1: a cached not-private verdict counts whatever the token setup ----

func TestVis_CachedNotPrivateVerdictHidesTheRepoWithoutAToken(t *testing.T) {
	api := newFakeAPI(t)
	visHost(t, api, nil)
	t.Setenv("MEMSYNC_GITHUB_TOKEN", "") // no token source at all
	rc, err := loadRelCtx("")
	if err != nil {
		t.Fatal(err)
	}
	writeVerdicts(t, rc.vis.cachePath, 10*time.Minute, false)
	if err := rc.vis.CheckPrivate("mem-project-b"); err == nil || !strings.Contains(err.Error(), "not private") {
		t.Fatalf("a fresh not-private verdict must win over names-only: %v", err)
	}
	if err := rc.vis.CheckCloneURL("mem-project-b", "git@github.com:Org/mem-project-b.git", nil); err == nil || !strings.Contains(err.Error(), "not private") {
		t.Fatalf("CheckCloneURL ignored the cached verdict: %v", err)
	}
	// With no verdict at all, the old names-only rule stands.
	os.Remove(rc.vis.cachePath)
	if err := rc.vis.CheckPrivate("mem-project-b"); err != nil {
		t.Fatalf("no token and no verdict is names only: %v", err)
	}
}

func writeVerdicts(t *testing.T, path string, age time.Duration, private bool) {
	t.Helper()
	at := time.Now().Add(-age)
	b, _ := json.Marshal(map[string]verdict{
		"org/mem-project-a": {Private: true, At: at},
		"org/mem-project-b": {Private: private, At: at},
	})
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// ---- S-5: offline, a cached not-private verdict hides the repo, fresh or stale ----

func TestVis_OfflineCachedNotPrivateVerdictHidesTheRepo(t *testing.T) {
	api := newFakeAPI(t)
	visHost(t, api, nil)
	for name, age := range map[string]time.Duration{"fresh": 5 * time.Minute, "2h old": 2 * time.Hour} {
		t.Run(name, func(t *testing.T) {
			rc, err := loadRelCtxHook("", nil)
			if err != nil {
				t.Fatal(err)
			}
			writeVerdicts(t, rc.vis.cachePath, age, false)
			err = rc.vis.CheckPrivate("mem-project-b")
			if err == nil || !strings.Contains(err.Error(), "not private") {
				t.Fatalf("offline, a cached not-private verdict (%s) must hide the repo: %v", name, err)
			}
			if api.count() != 0 {
				t.Errorf("the API was asked %d times", api.count())
			}
		})
	}
}

// ---- S-2: relatedMaxStaleHours is clamped to the 24 h cap ----

func TestVis_MaxStaleHoursIsClampedTo24(t *testing.T) {
	api := newFakeAPI(t)
	visHost(t, api, nil)
	p := os.Getenv(routesEnv)
	b, _ := os.ReadFile(p)
	var rj map[string]any
	_ = json.Unmarshal(b, &rj)
	rj["relatedMaxStaleHours"] = 1000
	b, _ = json.Marshal(rj)
	_ = os.WriteFile(p, b, 0o600)
	rc, err := loadRelCtx("")
	if err != nil {
		t.Fatal(err)
	}
	if rc.vis.maxStale > 24*time.Hour {
		t.Fatalf("maxStale = %v, want at most 24h", rc.vis.maxStale)
	}
	writeVerdicts(t, rc.vis.cachePath, 48*time.Hour, true)
	rc.vis.offline = true
	if err := rc.vis.CheckPrivate("mem-project-b"); err == nil {
		t.Fatal("a 48 h old verdict served although relatedMaxStaleHours is 1000")
	}
}

// ---- S-3: relatedCloneDirs fails closed ----

func TestRetireClassic_UnreadableRelatedRootKeepsTheIdentity(t *testing.T) {
	c := newClassicRepo(t)
	clone := relatedRekeyHost(t, c, "age1someoneelse")
	root := filepath.Dir(clone)
	// The root exists but cannot be listed: replace it with a plain file.
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, newRec := newPQKey(t, "newrec")
	code, out, e := rekey(t, c, "--recovery-recipient", newRec, "--host", "vm-1", "--fresh-root", "--apply")
	if code != 0 {
		t.Fatalf("%d %s%s", code, out, e)
	}
	if _, err := os.Stat(c.idPath); err != nil {
		t.Error("the classic identity was retired although the related root could not be read")
	}
	if !strings.Contains(out, "related") {
		t.Errorf("the output must say why the file is kept:\n%s", out)
	}
}

func TestRetireClassic_BrokenRoutesFileKeepsTheIdentity(t *testing.T) {
	c := newClassicRepo(t)
	relatedRekeyHost(t, c, "age1someoneelse")
	// routes.json now names a repo whose config is gone: loadRouted fails.
	p := os.Getenv(routesEnv)
	if err := os.WriteFile(p, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs := runRcpt("retire-classic", "--config", c.cfg.Path(), "--apply")
	_ = code
	if _, err := os.Stat(c.idPath); err != nil {
		t.Errorf("retire-classic retired the identity with an unusable routes file:\n%s%s", out, errs)
	}
}

// ---- S-4: host retire --apply respects the rekey guard ----

func TestHostRetire_ApplyRefusesARekeyBlockedRepoAndChangesNothing(t *testing.T) {
	f, cfg := hostFixture(t)
	e := newEncRepo(t, "vm-b")
	f.repos = []string{"mem-personal"}
	f.keys["mem-personal"] = []fakeKey{{1, "vm-b", false, hdays(9), hdays(1)}, {2, "vm-own", false, hdays(9), hdays(1)}}
	reg := newRegistryWithHost(t, "vm-b")
	hc := retireCtx(t, f, cfg, localRepo{Name: "mem-personal", Dir: e.cfg.SyncDir, Cfg: e.cfg})
	hc.registryDir = reg
	rec := filepath.Join(e.cfg.SyncDir, ".git", agecrypt.RekeyStateFile)
	if err := os.WriteFile(rec, []byte("{garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	headBefore := e.head(t)
	code, out, errb := hostDo(t, hc, "retire", "vm-b", "--apply")
	if code == 0 || !strings.Contains(errb, "finish or roll back the rekey first") {
		t.Fatalf("host retire --apply must refuse a blocked repo: %d\n%s%s", code, out, errb)
	}
	if e.head(t) != headBefore {
		t.Error("a commit was made on the blocked repo")
	}
	if n := f.nonGET(); len(n) != 0 {
		t.Errorf("mutating API calls were made: %v", n)
	}
}

func headRev(t *testing.T, dir string) string {
	t.Helper()
	h, err := gitwt.New(dir).Run("rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(h)
}
