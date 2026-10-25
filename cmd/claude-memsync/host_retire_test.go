package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/lock"
	"github.com/MarimerLLC/claude-utils/internal/testenv"
)

// encRepo is an encrypted sync repo of this host (label own), with a second
// host's recipient (label other) added and one note committed to both.
type encRepo struct {
	cfg     config.Config
	repo    *gitwt.Repo
	otherID string // the other host's identity file
	otherRC string
	oldBlob string // a note blob written while the other host was a recipient
}

func newEncRepo(t *testing.T, other string) *encRepo {
	t.Helper()
	cfg, _ := initFixture(t)
	t.Setenv("HOME", t.TempDir())
	ms, mm := testenv.Binaries(t)
	cfg.MergeDriverPath = mm
	cfg.HostLabel = "vm-own"
	enc := &encryptSpec{IdentityPath: filepath.Join(t.TempDir(), "k", "identity.txt"), MemsyncExe: ms}
	if err := bootstrapWith(cfg, false, nil, enc); err != nil {
		t.Fatal(err)
	}
	repo := gitwt.New(cfg.SyncDir)
	idB, rcB := newTestIdentity(t, "other")
	if err := agecrypt.AddRecipientLabelled(repo, rcB, other); err != nil {
		t.Fatal(err)
	}
	note := filepath.Join(cfg.SyncDir, "projects", "p1", "memory", "late.md")
	if err := os.WriteFile(note, []byte("written while both could read\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Run("add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("memsync: 1 files"); err != nil {
		t.Fatal(err)
	}
	blob, err := repo.Run("cat-file", "blob", "HEAD:projects/p1/memory/late.md")
	if err != nil {
		t.Fatal(err)
	}
	return &encRepo{cfg: cfg, repo: repo, otherID: idB, otherRC: rcB, oldBlob: blob}
}

func (e *encRepo) head(t *testing.T) string {
	t.Helper()
	h, err := e.repo.Run("rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(h)
}

func (e *encRepo) canRead(t *testing.T, idFile, blob string) bool {
	t.Helper()
	ids, err := agecrypt.LoadIdentities(idFile)
	if err != nil {
		t.Fatal(err)
	}
	_, err = agecrypt.Decrypt([]byte(blob), ids)
	return err == nil
}

func (e *encRepo) headBlob(t *testing.T) string {
	t.Helper()
	b, err := e.repo.Run("cat-file", "blob", "HEAD:projects/p1/memory/late.md")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// retireCtx is a context with the fake API, the encrypted repo as mem-personal
// and this host labelled vm-own.
func retireCtx(t *testing.T, f *fakeGH, base config.Config, locals ...localRepo) *hostCtx {
	t.Helper()
	return &hostCtx{cfg: base, org: "Org", local: locals, ownLabels: []string{"vm-own"}}
}

func idPathOf(t *testing.T, e *encRepo) string {
	t.Helper()
	return agecrypt.IdentityPath(e.repo)
}

func TestHostRetire_DryRunChangesNothing(t *testing.T) {
	f, cfg := hostFixture(t)
	e := newEncRepo(t, "vm-b")
	f.repos = []string{"mem-personal", "mem-project-x"}
	f.keys["mem-personal"] = []fakeKey{{1, "vm-b", false, hdays(9), hdays(1)}, {2, "vm-own", false, hdays(9), hdays(1)}}
	f.keys["mem-project-x"] = []fakeKey{{3, "vm-b", true, hdays(9), hdays(1)}}
	reg := newRegistryWithHost(t, "vm-b")
	hc := retireCtx(t, f, cfg, localRepo{Name: "mem-personal", Dir: e.cfg.SyncDir, Cfg: e.cfg})
	hc.registryDir = reg
	headBefore := e.head(t)
	regBefore, _ := execGit(reg, "rev-parse", "HEAD")
	recBefore, _ := os.ReadFile(filepath.Join(e.cfg.SyncDir, ".age-recipients"))

	code, out, errb := hostDo(t, hc, "retire", "vm-b")
	if code != 0 {
		t.Fatalf("%d\n%s%s", code, out, errb)
	}
	if n := f.nonGET(); len(n) != 0 {
		t.Errorf("dry-run must make no mutating API call: %v", n)
	}
	if e.head(t) != headBefore {
		t.Error("dry-run must not commit")
	}
	if after, _ := execGit(reg, "rev-parse", "HEAD"); after != regBefore {
		t.Error("dry-run must not commit to the registry")
	}
	if rec, _ := os.ReadFile(filepath.Join(e.cfg.SyncDir, ".age-recipients")); !bytes.Equal(rec, recBefore) {
		t.Error("dry-run must not touch the recipients")
	}
	if _, err := os.Stat(filepath.Join(reg, "hosts", "vm-b")); err != nil {
		t.Error("dry-run must not remove the registry entry")
	}
	for _, want := range []string{"dry run", "deploy keys to delete: 2", "mem-personal", "mem-project-x", "--apply"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan lacks %q:\n%s", want, out)
		}
	}
	// the explicit flag behaves the same, and the two flags conflict
	if code, _, _ := hostDo(t, hc, "retire", "vm-b", "--dry-run"); code != 0 {
		t.Error("--dry-run should be accepted")
	}
	if code, _, errb := hostDo(t, hc, "retire", "vm-b", "--dry-run", "--apply"); code != 2 {
		t.Errorf("--dry-run with --apply must be a usage error: %d %s", code, errb)
	}
	if len(f.nonGET()) != 0 {
		t.Error("still no mutations")
	}
}

func newRegistryWithHost(t *testing.T, label string) string {
	t.Helper()
	reg := t.TempDir()
	if out, err := execGit(reg, "init", "-q", "-b", "main"); err != nil {
		t.Fatal(out, err)
	}
	p := filepath.Join(reg, "hosts", label)
	if err := os.MkdirAll(p, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "routes.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := execGit(reg, "add", "-A"); err != nil {
		t.Fatal(out, err)
	}
	if out, err := execGit(reg, "commit", "-q", "-m", "seed"); err != nil {
		t.Fatal(out, err)
	}
	return reg
}

func TestHostRetire_ApplyDeletesOnlyThatLabelsKeysOnMemRepos(t *testing.T) {
	f, cfg := hostFixture(t)
	f.repos = []string{"mem-personal", "mem-project-x", "website", "mem-bogus"}
	f.keys["mem-personal"] = []fakeKey{{1, "vm-b", false, hdays(9), hdays(1)}, {2, "vm-own", false, hdays(9), hdays(1)}, {4, "vm-bb", false, hdays(9), hdays(1)}}
	f.keys["mem-project-x"] = []fakeKey{{3, "vm-b", true, hdays(9), hdays(1)}, {5, "VM-B", true, hdays(9), hdays(1)}}
	f.keys["website"] = []fakeKey{{9, "vm-b", false, hdays(1), hdays(1)}}
	f.keys["mem-bogus"] = []fakeKey{{8, "vm-b", false, hdays(1), hdays(1)}}
	hc := retireCtx(t, f, cfg)
	code, out, errb := hostDo(t, hc, "retire", "vm-b", "--apply")
	if code != 0 {
		t.Fatalf("%d\n%s%s", code, out, errb)
	}
	got := strings.Join(f.deletes(), ",")
	if got != "DELETE /repos/Org/mem-personal/keys/1,DELETE /repos/Org/mem-project-x/keys/3" {
		t.Errorf("deleted: %s", got)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "/website") || strings.Contains(c, "mem-bogus") {
			t.Errorf("a repo outside the allowed mem-* names was touched: %s", c)
		}
	}
	if len(f.keys["mem-personal"]) != 2 || len(f.keys["mem-project-x"]) != 1 {
		t.Errorf("other labels' keys must remain: %+v", f.keys)
	}
	if strings.Contains(out, "AAAA") || strings.Contains(out, cmdTok) {
		t.Error("no key material in the output")
	}
}

func TestHostRetire_RemovesRecipientAndReencrypts(t *testing.T) {
	f, cfg := hostFixture(t)
	e := newEncRepo(t, "vm-b")
	f.repos = []string{"mem-personal"}
	f.keys["mem-personal"] = []fakeKey{{1, "vm-b", false, hdays(9), hdays(1)}}
	hc := retireCtx(t, f, cfg, localRepo{Name: "mem-personal", Dir: e.cfg.SyncDir, Cfg: e.cfg})
	if !e.canRead(t, e.otherID, e.headBlob(t)) {
		t.Fatal("setup: the other host should read HEAD before retirement")
	}
	headBefore := e.head(t)
	code, out, errb := hostDo(t, hc, "retire", "vm-b", "--apply")
	if code != 0 {
		t.Fatalf("%d\n%s%s", code, out, errb)
	}
	if e.head(t) == headBefore {
		t.Error("retire should commit the re-encryption")
	}
	labels, _ := agecrypt.ListLabels(e.cfg.SyncDir)
	if strings.Join(labels, ",") != "vm-own" {
		t.Errorf("labels after retire: %v", labels)
	}
	if e.canRead(t, e.otherID, e.headBlob(t)) {
		t.Error("the retired key must not read HEAD's blobs")
	}
	if !e.canRead(t, idPathOf(t, e), e.headBlob(t)) {
		t.Error("this host must still read HEAD")
	}
	if !e.canRead(t, e.otherID, e.oldBlob) {
		t.Error("history is not rewritten: the old blob stays readable to the retired key")
	}
	if !strings.Contains(out, "history stays readable") {
		t.Errorf("the output must say history is not rewritten:\n%s", out)
	}
	if d, _ := e.repo.Run("status", "--porcelain"); strings.TrimSpace(d) != "" {
		t.Errorf("dirty after retire: %s", d)
	}
}

func TestHostRetire_ListsReposItCannotReencrypt(t *testing.T) {
	f, cfg := hostFixture(t)
	e := newEncRepo(t, "vm-b")
	// A second local repo this host cannot read (keyless): its recipients name vm-b.
	keyless := newKeylessRepoNamingLabel(t, "vm-b")
	f.repos = []string{"mem-personal", "mem-project-k", "mem-project-away", "mem-project-none"}
	f.keys["mem-personal"] = []fakeKey{{1, "vm-b", false, hdays(9), hdays(1)}}
	f.keys["mem-project-away"] = []fakeKey{{2, "vm-b", true, hdays(9), hdays(1)}} // not a repo of this host
	f.keys["mem-project-none"] = []fakeKey{{3, "vm-other", true, hdays(9), hdays(1)}}
	hc := retireCtx(t, f, cfg,
		localRepo{Name: "mem-personal", Dir: e.cfg.SyncDir, Cfg: e.cfg},
		localRepo{Name: "mem-project-k", Dir: keyless.SyncDir, Cfg: keyless})
	code, out, errb := hostDo(t, hc, "retire", "vm-b", "--apply")
	if code != 0 {
		t.Fatalf("%d\n%s%s", code, out, errb)
	}
	i := strings.Index(out, "cannot re-encrypt here")
	if i < 0 {
		t.Fatalf("no can't-re-encrypt list:\n%s", out)
	}
	list := out[i:]
	var items []string
	for _, ln := range strings.Split(list, "\n")[1:] {
		if !strings.HasPrefix(ln, "  ") {
			break
		}
		items = append(items, ln)
	}
	itemText := strings.Join(items, "\n")
	if !strings.Contains(itemText, "mem-project-k") || !strings.Contains(itemText, "mem-project-away") {
		t.Errorf("both unreadable and absent repos belong in the list:\n%s", list)
	}
	if strings.Contains(itemText, "mem-project-none") || strings.Contains(itemText, "mem-personal") {
		t.Errorf("repos with nothing to re-encrypt, or done, must not be listed:\n%s", list)
	}
	if !strings.Contains(list, "run retire on a member host, or archive the repo") {
		t.Errorf("the advice is missing:\n%s", list)
	}
	labels, _ := agecrypt.ListLabels(keyless.SyncDir)
	if strings.Join(labels, ",") != "vm-b" {
		t.Errorf("a repo this host cannot read must be left alone: %v", labels)
	}
	// the keys were still deleted on both repos
	if len(f.deletes()) != 2 {
		t.Errorf("deletes: %v", f.deletes())
	}
}

// newKeylessRepoNamingLabel makes an encrypted clone this host has no key for,
// whose .age-recipients holds only a line labelled label.
func newKeylessRepoNamingLabel(t *testing.T, label string) config.Config {
	t.Helper()
	cfg, _ := initFixture(t)
	ms, mm := testenv.Binaries(t)
	cfg.MergeDriverPath = mm
	cfg.SyncDir = filepath.Join(t.TempDir(), "keyless")
	// first host creates the repo; a second clone (ours) has another identity
	first := &encryptSpec{IdentityPath: filepath.Join(t.TempDir(), "a"), MemsyncExe: ms}
	if err := bootstrapWith(cfg, false, nil, first); err != nil {
		t.Fatal(err)
	}
	repoA := gitwt.New(cfg.SyncDir)
	_, rcL := newTestIdentity(t, "lab")
	if err := agecrypt.AddRecipientLabelled(repoA, rcL, label); err != nil {
		t.Fatal(err)
	}
	if err := repoA.Push("main"); err != nil {
		t.Fatal(err)
	}
	cfg2 := cfg
	cfg2.SyncDir = filepath.Join(t.TempDir(), "mine")
	cfg2.ClaudeProjectsDir = filepath.Join(t.TempDir(), "claude2")
	second := &encryptSpec{IdentityPath: filepath.Join(t.TempDir(), "b"), MemsyncExe: ms}
	if err := bootstrapWith(cfg2, false, nil, second); err != nil {
		t.Fatal(err)
	}
	if agecrypt.CheckKeyed(gitwt.New(cfg2.SyncDir)) == nil {
		t.Fatal("setup: the clone should be keyless")
	}
	return cfg2
}

func TestHostRetire_RefusesOwnLabel(t *testing.T) {
	f, cfg := hostFixture(t)
	f.repos = []string{"mem-personal"}
	f.keys["mem-personal"] = []fakeKey{{1, "vm-own", false, hdays(9), hdays(1)}}
	hc := retireCtx(t, f, cfg)
	for _, args := range [][]string{{"retire", "vm-own"}, {"retire", "vm-own", "--apply"}} {
		code, _, errb := hostDo(t, hc, args...)
		if code != 1 || !strings.Contains(errb, "this host") {
			t.Errorf("%v: must refuse the current host's own label: %d %s", args, code, errb)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("a refused retire must not call the API: %v", f.calls)
	}
	if code, _, _ := hostDo(t, hc, "retire", "Not A Label"); code == 0 {
		t.Error("an invalid label must be refused")
	}
	if code, _, _ := hostDo(t, hc, "retire"); code != 2 {
		t.Error("a label is required")
	}
}

func TestHostRetire_InsufficientPermissionIsExplainedAndNothingElseHappens(t *testing.T) {
	f, cfg := hostFixture(t)
	e := newEncRepo(t, "vm-b")
	f.repos = []string{"mem-personal"}
	f.keys["mem-personal"] = []fakeKey{{1, "vm-b", false, hdays(9), hdays(1)}}
	f.forbidDelete = true
	reg := newRegistryWithHost(t, "vm-b")
	hc := retireCtx(t, f, cfg, localRepo{Name: "mem-personal", Dir: e.cfg.SyncDir, Cfg: e.cfg})
	hc.registryDir = reg
	headBefore := e.head(t)
	code, out, errb := hostDo(t, hc, "retire", "vm-b", "--apply")
	if code != 1 || !strings.Contains(errb, "Administration: write") {
		t.Fatalf("want a clear permission message, got %d\n%s%s", code, out, errb)
	}
	if e.head(t) != headBefore {
		t.Error("recipients must not be touched when the keys could not be deleted")
	}
	if _, err := os.Stat(filepath.Join(reg, "hosts", "vm-b")); err != nil {
		t.Error("the registry entry must stay")
	}
}

func TestHostRetire_RefusesWhileDaemonHoldsTheSyncDir(t *testing.T) {
	f, cfg := hostFixture(t)
	e := newEncRepo(t, "vm-b")
	f.repos = []string{"mem-personal"}
	f.keys["mem-personal"] = []fakeKey{{1, "vm-b", false, hdays(9), hdays(1)}}
	l, err := lock.Acquire(e.cfg.SyncDir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	hc := retireCtx(t, f, cfg, localRepo{Name: "mem-personal", Dir: e.cfg.SyncDir, Cfg: e.cfg})
	code, _, errb := hostDo(t, hc, "retire", "vm-b", "--apply")
	if code != 1 || !strings.Contains(errb, "claude-memsync stop") {
		t.Errorf("want the stop-the-daemon advice: %d %s", code, errb)
	}
	if len(f.deletes()) != 0 {
		t.Error("nothing may be deleted before the re-encryption can run")
	}
}

func TestHostRetire_RegistryEntryAndCheckKeyManualStep(t *testing.T) {
	f, cfg := hostFixture(t)
	cfg.GitHubHost = "git.example.test"
	f.repos = []string{"mem-personal"}
	f.keys["mem-personal"] = []fakeKey{{1, "vm-b", false, hdays(9), hdays(1)}}
	reg := newRegistryWithHost(t, "vm-b")
	fp := "SHA256:" + strings.Repeat("k", 43)
	if err := os.WriteFile(filepath.Join(reg, "hosts", "vm-b", "check-key"), []byte(`{"fingerprint":"`+fp+`","app":"check-app"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	execGit(reg, "add", "-A")
	execGit(reg, "commit", "-q", "-m", "ck")
	hc := retireCtx(t, f, cfg)
	hc.registryDir = reg
	wantURL := "https://git.example.test/organizations/Org/settings/apps/check-app"
	const caveat = "as recorded in the registry (verify the fingerprint in GitHub's UI before deleting)"
	// dry run: the manual step is announced, nothing removed
	code, out, _ := hostDo(t, hc, "retire", "vm-b")
	if code != 0 || !strings.Contains(out, wantURL) || !strings.Contains(out, fp) || !strings.Contains(out, caveat) {
		t.Errorf("plan should name the URL and fingerprint, with the caveat:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(reg, "hosts", "vm-b", "check-key")); err != nil {
		t.Error("dry-run removed the record")
	}
	code, out, errb := hostDo(t, hc, "retire", "vm-b", "--apply")
	if code != 0 {
		t.Fatalf("%d %s%s", code, out, errb)
	}
	if !strings.Contains(out, wantURL) || !strings.Contains(out, fp) || !strings.Contains(out, "manual") || !strings.Contains(out, caveat) {
		t.Errorf("manual step missing:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(reg, "hosts", "vm-b")); err == nil {
		t.Error("hosts/vm-b/ should be removed from the registry")
	}
	if st, _ := execGit(reg, "status", "--porcelain"); strings.TrimSpace(st) != "" {
		t.Errorf("the removal must be committed: %s", st)
	}
	if pushed, _ := execGit(reg, "remote"); strings.TrimSpace(pushed) != "" {
		t.Error("test registry should have no remote")
	}
}

func TestHostRetire_NoCheckKeyPrintsNoManualStep(t *testing.T) {
	f, cfg := hostFixture(t)
	f.repos = []string{"mem-personal"}
	f.keys["mem-personal"] = []fakeKey{{1, "vm-b", false, hdays(9), hdays(1)}}
	reg := newRegistryWithHost(t, "vm-b")
	hc := retireCtx(t, f, cfg)
	hc.registryDir = reg
	_, out, _ := hostDo(t, hc, "retire", "vm-b", "--apply")
	if strings.Contains(out, "organizations") || strings.Contains(out, "manual") {
		t.Errorf("no check key recorded: print nothing about it:\n%s", out)
	}
}

func TestHostRetire_UnknownLabelIsANoOp(t *testing.T) {
	f, cfg := hostFixture(t)
	f.repos = []string{"mem-personal"}
	f.keys["mem-personal"] = []fakeKey{{1, "vm-b", false, hdays(9), hdays(1)}}
	hc := retireCtx(t, f, cfg)
	code, out, errb := hostDo(t, hc, "retire", "vm-typo", "--apply")
	if code != 0 || !strings.Contains(out, "nothing to retire") || len(f.deletes()) != 0 {
		t.Errorf("%d %s%s", code, out, errb)
	}
}

func recipientText(t *testing.T, e *encRepo) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(e.cfg.SyncDir, ".age-recipients"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestHostRetire_SwappedLabelsAreUnreviewedAndRemoveNothing(t *testing.T) {
	f, cfg := hostFixture(t)
	e := newEncRepo(t, "vm-b")
	f.repos = []string{"mem-personal"}
	f.keys["mem-personal"] = []fakeKey{{1, "vm-b", false, hdays(9), hdays(1)}}
	// Upstream swaps the comments: vm-b now names this host's own key.
	lines, _ := agecrypt.ListRecipients(e.cfg.SyncDir)
	own := strings.Fields(lines[0])[0]
	if err := agecrypt.WriteRecipients(e.cfg.SyncDir, []string{own + " # host:vm-b", e.otherRC + " # host:vm-own"}); err != nil {
		t.Fatal(err)
	}
	hc := retireCtx(t, f, cfg, localRepo{Name: "mem-personal", Dir: e.cfg.SyncDir, Cfg: e.cfg})
	headBefore := e.head(t)
	code, out, errb := hostDo(t, hc, "retire", "vm-b", "--apply")
	if code != 0 {
		t.Fatalf("%d %s%s", code, out, errb)
	}
	if e.head(t) != headBefore || !strings.Contains(recipientText(t, e), e.otherRC) || !strings.Contains(recipientText(t, e), own) {
		t.Error("with unreviewed recipients nothing may be removed from them")
	}
	i := strings.Index(out, "cannot re-encrypt here")
	if i < 0 || !strings.Contains(out[i:], "mem-personal") || !strings.Contains(out, "not reviewed") {
		t.Errorf("the unreviewed repo belongs in the can't-re-encrypt list:\n%s", out)
	}
}

func TestHostRetire_RefusesALabelOnTwoKeys(t *testing.T) {
	f, cfg := hostFixture(t)
	e := newEncRepo(t, "vm-b")
	f.repos = []string{"mem-personal"}
	f.keys["mem-personal"] = []fakeKey{{1, "vm-b", false, hdays(9), hdays(1)}}
	_, rcX := newTestIdentity(t, "x")
	lines, _ := agecrypt.ListRecipients(e.cfg.SyncDir)
	if err := agecrypt.WriteRecipients(e.cfg.SyncDir, append(lines, rcX+" # host:vm-b")); err != nil {
		t.Fatal(err)
	}
	if err := agecrypt.AcceptRecipients(e.repo); err != nil { // a person accepted the duplicate
		t.Fatal(err)
	}
	hc := retireCtx(t, f, cfg, localRepo{Name: "mem-personal", Dir: e.cfg.SyncDir, Cfg: e.cfg})
	for _, args := range [][]string{{"retire", "vm-b"}, {"retire", "vm-b", "--apply"}} {
		code, _, errb := hostDo(t, hc, args...)
		if code != 1 || !strings.Contains(errb, "more than one key") {
			t.Errorf("%v: want a refusal: %d %s", args, code, errb)
		}
	}
	if len(f.deletes()) != 0 {
		t.Errorf("a refused retire deletes nothing: %v", f.deletes())
	}
}

func TestHostRetire_RefusesALabelOnDifferentKeysAcrossRepos(t *testing.T) {
	f, cfg := hostFixture(t)
	e1 := newEncRepo(t, "vm-b")
	e2 := newEncRepo(t, "vm-b") // a different identity behind the same label
	f.repos = []string{"mem-personal", "mem-project-x"}
	f.keys["mem-personal"] = []fakeKey{{1, "vm-b", false, hdays(9), hdays(1)}}
	hc := retireCtx(t, f, cfg,
		localRepo{Name: "mem-personal", Dir: e1.cfg.SyncDir, Cfg: e1.cfg},
		localRepo{Name: "mem-project-x", Dir: e2.cfg.SyncDir, Cfg: e2.cfg})
	h1, h2 := e1.head(t), e2.head(t)
	code, _, errb := hostDo(t, hc, "retire", "vm-b", "--apply")
	if code != 1 || !strings.Contains(errb, "different keys") {
		t.Fatalf("want a refusal: %d %s", code, errb)
	}
	if len(f.deletes()) != 0 || e1.head(t) != h1 || e2.head(t) != h2 {
		t.Error("a refused retire changes nothing")
	}
}

func TestHostRetire_PlanShowsTheKeyFingerprintPerRepo(t *testing.T) {
	f, cfg := hostFixture(t)
	e := newEncRepo(t, "vm-b")
	f.repos = []string{"mem-personal"}
	f.keys["mem-personal"] = []fakeKey{{1, "vm-b", false, hdays(9), hdays(1)}}
	hc := retireCtx(t, f, cfg, localRepo{Name: "mem-personal", Dir: e.cfg.SyncDir, Cfg: e.cfg})
	_, out, _ := hostDo(t, hc, "retire", "vm-b")
	keys, _ := agecrypt.PinnedKeysForLabel(e.repo, "vm-b")
	if !strings.Contains(out, agecrypt.KeyPrint(keys[0])) {
		t.Errorf("the plan should show the short fingerprint of the key it would remove:\n%s", out)
	}
	if strings.Contains(out, e.otherRC) {
		t.Error("never the key itself")
	}
}

func TestHostRetire_HoldsEveryInvolvedLockForTheWholeApply(t *testing.T) {
	f, cfg := hostFixture(t)
	e := newEncRepo(t, "vm-b")
	f.repos = []string{"mem-personal"}
	f.keys["mem-personal"] = []fakeKey{{1, "vm-b", false, hdays(9), hdays(1)}}
	var heldDuringDelete bool
	f.onDelete = func(string) {
		l, err := lock.Acquire(e.cfg.SyncDir)
		if err == nil {
			l.Release()
			return
		}
		heldDuringDelete = errors.Is(err, lock.ErrHeld)
	}
	hc := retireCtx(t, f, cfg, localRepo{Name: "mem-personal", Dir: e.cfg.SyncDir, Cfg: e.cfg})
	// ... and also while the recipients are being rewritten
	var heldDuringRewrite bool
	old := removeLabelRecipient
	removeLabelRecipient = func(r *gitwt.Repo, label string) error {
		if l, err := lock.Acquire(r.Dir); err == nil {
			l.Release()
		} else {
			heldDuringRewrite = errors.Is(err, lock.ErrHeld)
		}
		return old(r, label)
	}
	t.Cleanup(func() { removeLabelRecipient = old })
	if code, out, errb := hostDo(t, hc, "retire", "vm-b", "--apply"); code != 0 {
		t.Fatalf("%d %s%s", code, out, errb)
	}
	if !heldDuringDelete || !heldDuringRewrite {
		t.Errorf("the daemon lock must be held across the whole apply: delete=%v rewrite=%v", heldDuringDelete, heldDuringRewrite)
	}
	l, err := lock.Acquire(e.cfg.SyncDir)
	if err != nil {
		t.Fatalf("the lock must be released afterwards: %v", err)
	}
	l.Release()
}

func TestHostRetire_PartialFailureIsSummarisedAndARerunFinishesIt(t *testing.T) {
	f, cfg := hostFixture(t)
	e := newEncRepo(t, "vm-b")
	f.repos = []string{"mem-personal"}
	f.keys["mem-personal"] = []fakeKey{{1, "vm-b", false, hdays(9), hdays(1)}}
	reg := newRegistryWithHost(t, "vm-b")
	hc := retireCtx(t, f, cfg, localRepo{Name: "mem-personal", Dir: e.cfg.SyncDir, Cfg: e.cfg})
	hc.registryDir = reg
	old := removeLabelRecipient
	removeLabelRecipient = func(*gitwt.Repo, string) error { return errors.New("simulated failure") }
	t.Cleanup(func() { removeLabelRecipient = old })
	code, out, errb := hostDo(t, hc, "retire", "vm-b", "--apply")
	if code != 1 || !strings.Contains(out, "STILL A RECIPIENT in mem-personal: re-run") {
		t.Fatalf("the summary must list what is pending: %d\n%s%s", code, out, errb)
	}
	if len(f.keys["mem-personal"]) != 0 {
		t.Error("the key deletion step had already happened")
	}
	removeLabelRecipient = old
	code, out, errb = hostDo(t, hc, "retire", "vm-b", "--apply")
	if code != 0 || strings.Contains(out, "STILL") || !strings.Contains(out, "retire complete") {
		t.Fatalf("the re-run should finish the job: %d\n%s%s", code, out, errb)
	}
	labels, _ := agecrypt.ListLabels(e.cfg.SyncDir)
	if strings.Join(labels, ",") != "vm-own" {
		t.Errorf("labels: %v", labels)
	}
	if _, err := os.Stat(filepath.Join(reg, "hosts", "vm-b")); err == nil {
		t.Error("the registry entry should be gone")
	}
	// and a third run changes nothing at all
	headAfter, deletesAfter := e.head(t), len(f.deletes())
	regHead, _ := execGit(reg, "rev-parse", "HEAD")
	code, out, _ = hostDo(t, hc, "retire", "vm-b", "--apply")
	if code != 0 || !strings.Contains(out, "nothing to retire") {
		t.Errorf("a repeat is a no-op: %d %s", code, out)
	}
	if e.head(t) != headAfter || len(f.deletes()) != deletesAfter {
		t.Error("a repeat must change nothing")
	}
	if h, _ := execGit(reg, "rev-parse", "HEAD"); h != regHead {
		t.Error("a repeat must not commit to the registry")
	}
}
