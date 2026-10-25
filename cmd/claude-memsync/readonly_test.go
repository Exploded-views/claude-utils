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

func originHeadOf(t *testing.T, origin string) string {
	t.Helper()
	h, err := gitwt.New(origin).Run("rev-parse", "main")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(h)
}

func TestInitReadOnly_PopulatesClaudeButNeverCommitsOrPushes(t *testing.T) {
	cfg, origin := initFixture(t)
	if err := bootstrap(cfg, false, nil); err != nil { // a writer seeds the remote
		t.Fatal(err)
	}
	before := originHeadOf(t, origin)

	ro := cfg
	ro.SyncDir = filepath.Join(t.TempDir(), "ro-sync")
	ro.ClaudeProjectsDir = filepath.Join(t.TempDir(), "ro-claude")
	ro.ReadOnly = true
	mem := filepath.Join(ro.ClaudeProjectsDir, "p9", "memory")
	if err := os.MkdirAll(mem, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mem, "local-only.md"), []byte("never leaves\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := bootstrap(ro, false, nil); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(ro.ClaudeProjectsDir, "p1", "memory", "ok.md"))
	if err != nil || string(b) != "harmless\n" {
		t.Errorf("the remote's notes must reach the Claude tree: %q %v", b, err)
	}
	if got := originHeadOf(t, origin); got != before {
		t.Error("a read-only init pushed")
	}
	if strings.Contains(originFiles(t, origin), "local-only.md") {
		t.Error("a local note reached the remote")
	}
	roHead, _ := gitwt.New(ro.SyncDir).Run("rev-parse", "HEAD")
	if strings.TrimSpace(roHead) != before {
		t.Errorf("a read-only init must not commit: HEAD %s, remote %s", roHead, before)
	}
	saved, err := config.Load(ro.Path())
	if err != nil || !saved.ReadOnly {
		t.Errorf("readOnly must be saved to config.json: %+v %v", saved, err)
	}
}

func TestInitReadOnly_RefusesAnEmptyRemote(t *testing.T) {
	cfg, origin := initFixture(t)
	cfg.ReadOnly = true
	err := bootstrap(cfg, false, nil)
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("a read-only host cannot create a repo's first commit: %v", err)
	}
	if out, _ := gitwt.New(origin).Run("rev-parse", "--verify", "-q", "main"); strings.TrimSpace(out) != "" {
		t.Error("nothing may be pushed")
	}
}

func TestInitReadOnly_FlagIsParsedAndKeptOnReinit(t *testing.T) {
	cfg, _ := initFixture(t)
	cfg.ReadOnly = true
	if err := os.MkdirAll(cfg.SyncDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if got := mergeReinit(config.Defaults(), cfg.Path()); !got.ReadOnly {
		t.Error("a re-init without the flag must not make a read-only host writable")
	}
}

func TestStatus_ShowsReadOnly(t *testing.T) {
	cfg := config.Defaults()
	if readOnlyLine(cfg) != "" {
		t.Error("a writable host says nothing")
	}
	cfg.ReadOnly = true
	if l := readOnlyLine(cfg); !strings.Contains(l, "read-only") {
		t.Errorf("status line: %q", l)
	}
}

func TestRecipients_ReadOnlyRefusesAddRemoveAndAcceptOnlyPins(t *testing.T) {
	e := newEncRepo(t, "vm-b")
	e.cfg.ReadOnly = true
	if err := config.Save(e.cfg); err != nil {
		t.Fatal(err)
	}
	_, rc := newTestIdentity(t, "third")
	headBefore := e.head(t)
	for _, args := range [][]string{{"add", rc, "--host", "vm-c"}, {"remove", e.otherRC}} {
		var out, errb bytes.Buffer
		code := recipientsCmd(&out, &errb, append(args, "--config", e.cfg.Path()))
		if code == 0 || !strings.Contains(errb.String(), "read-only") {
			t.Errorf("%v must be refused: %d %s", args, code, errb.String())
		}
	}
	if e.head(t) != headBefore {
		t.Error("a refused command committed")
	}
	// An upstream recipients change is pinned, not re-encrypted and committed.
	_, rc2 := newTestIdentity(t, "fourth")
	lines, _ := agecrypt.ListRecipients(e.cfg.SyncDir)
	if err := agecrypt.WriteRecipients(e.cfg.SyncDir, append(lines, rc2)); err != nil {
		t.Fatal(err)
	}
	if u, _ := agecrypt.Unreviewed(e.repo); !u {
		t.Fatal("setup: the change should be unreviewed")
	}
	var out, errb bytes.Buffer
	if code := recipientsCmd(&out, &errb, []string{"accept", "--yes", "--config", e.cfg.Path()}); code != 0 {
		t.Fatalf("%d %s", code, errb.String())
	}
	if u, _ := agecrypt.Unreviewed(e.repo); u {
		t.Error("accept should pin")
	}
	if e.head(t) != headBefore {
		t.Error("accept on a read-only host must not commit a re-encryption")
	}
}

func TestHost_ReadOnlyRegistryAndRepos(t *testing.T) {
	f, cfg := hostFixture(t)
	e := newEncRepo(t, "vm-b")
	roCfg := e.cfg
	roCfg.ReadOnly = true
	f.repos = []string{"mem-personal"}
	f.keys["mem-personal"] = []fakeKey{{1, "vm-b", false, hdays(9), hdays(1)}}
	reg := newRegistryWithHost(t, "vm-b")
	hc := retireCtx(t, f, cfg, localRepo{Name: "mem-personal", Dir: e.cfg.SyncDir, Cfg: roCfg})
	hc.registryDir, hc.registryReadOnly = reg, true
	headBefore := e.head(t)
	code, out, errb := hostDo(t, hc, "retire", "vm-b", "--apply")
	if code != 0 {
		t.Fatalf("%d %s%s", code, out, errb)
	}
	if e.head(t) != headBefore {
		t.Error("a read-only repo must not be re-encrypted")
	}
	if !strings.Contains(out, "cannot re-encrypt here") || !strings.Contains(out, "read-only") {
		t.Errorf("the read-only repo belongs in the can't-re-encrypt list:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(reg, "hosts", "vm-b")); err != nil {
		t.Error("a read-only registry must keep its entry")
	}
	if len(f.deletes()) != 1 {
		t.Errorf("the deploy key is still deleted: %v", f.deletes())
	}
	// check-key refuses to commit into a read-only registry
	hc.ownLabels = []string{"vm-own"}
	fp := "SHA256:" + strings.Repeat("Y", 43)
	if code, _, errb := hostDo(t, hc, "check-key", "--fingerprint", fp, "--app", "chk"); code == 0 || !strings.Contains(errb, "read-only") {
		t.Errorf("check-key must refuse: %d %s", code, errb)
	}
}

func TestRegistryCommitsRefuseOnAReadOnlyRegistry(t *testing.T) {
	h := newRelHost(t, map[string][2]string{"a": {"mem-project-a", "project"}}, nil)
	reg := h.cfgs["registry"]
	reg.ReadOnly = true
	if err := config.Save(reg); err != nil {
		t.Fatal(err)
	}
	before, _ := gitwt.New(reg.SyncDir).Run("rev-parse", "HEAD")
	if code, _, errs := relate("mem-project-a", "mem-project-b", "--read"); code == 0 || !strings.Contains(errs, "read-only") {
		t.Errorf("relate must refuse: %d %s", code, errs)
	}
	var out, errb bytes.Buffer
	if code := routePublish([]string{"--as", "vm-x"}, &out, &errb); code == 0 || !strings.Contains(errb.String(), "read-only") {
		t.Errorf("route publish must refuse: %d %s", code, errb.String())
	}
	if after, _ := gitwt.New(reg.SyncDir).Run("rev-parse", "HEAD"); after != before {
		t.Error("the registry got a commit")
	}
}
