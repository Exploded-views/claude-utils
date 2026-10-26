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
	"github.com/MarimerLLC/claude-utils/internal/testenv"
)

func newPQKey(t *testing.T, name string) (string, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	r, err := agecrypt.GenerateHybridIdentity(p)
	if err != nil {
		t.Fatal(err)
	}
	return p, r
}

// secretOf reads the secret line of an identity file (test-only).
func secretOf(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "AGE-SECRET-KEY-") {
			return l
		}
	}
	t.Fatal("no secret in identity file")
	return ""
}

func TestInit_PQIsTheDefaultAndNeverPrintsTheSecret(t *testing.T) {
	cfg, origin := initFixture(t)
	t.Setenv("HOME", t.TempDir())
	ms, mm := testenv.Binaries(t)
	cfg.MergeDriverPath = mm
	_, rec := newPQKey(t, "rec")
	idp := filepath.Join(t.TempDir(), "k", "identity.txt")
	enc := &encryptSpec{IdentityPath: idp, MemsyncExe: ms, Scheme: agecrypt.SchemePQ, Recipients: []string{rec + " # recovery"}}
	cfg.RecoveryRecipients = []string{rec}
	var err error
	out := captureStderr(t, func() { err = bootstrapWith(cfg, false, nil, enc) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, secretOf(t, idp)) || strings.Contains(out, "AGE-SECRET-KEY") {
		t.Fatal("the secret identity was printed")
	}
	fi, _ := os.Stat(idp)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("identity mode %v", fi.Mode())
	}
	own, _ := agecrypt.IdentityRecipient(idp)
	if !strings.HasPrefix(own, "age1pq1") || !strings.Contains(out, "public recipient: "+own) {
		t.Error("only the public hybrid recipient is printed")
	}
	c, _ := config.Load(cfg.Path())
	repo := gitwt.New(cfg.SyncDir)
	if c.EncryptionScheme != "pq" || agecrypt.SchemeOf(repo) != agecrypt.SchemePQ {
		t.Errorf("scheme not recorded: %q", c.EncryptionScheme)
	}
	// round trip through the filter: origin holds hybrid ciphertext only
	n := 0
	files, _ := gitwt.New(origin).Run("ls-tree", "-r", "--name-only", "main")
	for _, f := range strings.Fields(files) {
		if agecrypt.Exempt(f) {
			continue
		}
		b, _ := gitwt.New(origin).Run("cat-file", "blob", "main:"+f)
		if !agecrypt.IsHybridCiphertext([]byte(b)) {
			t.Errorf("%s is not hybrid ciphertext", f)
		}
		n++
	}
	if n == 0 {
		t.Fatal("no files pushed")
	}
	if d, _ := repo.Run("status", "--porcelain"); strings.TrimSpace(d) != "" {
		t.Errorf("dirty: %s", d)
	}
}

func TestInit_PQRefusesClassicAndSSHRecipients(t *testing.T) {
	_, cl := newTestIdentity(t, "c")
	ssh := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHcbV3DnP0R7ZqzuZXlzJ4W0eKgQh8WZ1xX3nYF8mO6c"
	_, pq := newPQKey(t, "p")
	for _, a := range [][]string{
		{"--recipient", cl}, {"--recovery-recipient", cl}, {"--recipient", ssh}, {"--recovery-recipient", ssh},
		{"--recipient", pq, "--recovery-recipient", cl},
	} {
		args := append([]string{"--remote", "/nonexistent/x.git", "--encrypt"}, a...)
		var code int
		out := captureStderr(t, func() { code = runInit(args) })
		if code != 2 || !strings.Contains(out, "post-quantum") {
			t.Errorf("%v: exit %d, %q", a, code, out)
		}
	}
	// classic mode still takes them (it then fails later on the missing remote, not on the key)
	args := []string{"--remote", "/nonexistent/x.git", "--encrypt", "--encryption-scheme", "classic", "--recipient", cl, "--sync-dir", filepath.Join(t.TempDir(), "s")}
	out := captureStderr(t, func() { runInit(args) })
	if strings.Contains(out, "post-quantum") {
		t.Errorf("classic mode must accept classic keys: %q", out)
	}
	// a hybrid key is refused in classic mode (age cannot mix them)
	args = []string{"--remote", "/nonexistent/x.git", "--encrypt", "--encryption-scheme", "classic", "--recipient", pq}
	var code int
	captureStderr(t, func() { code = runInit(args) })
	if code != 2 {
		t.Errorf("hybrid recipient in classic mode: exit %d", code)
	}
}

// classicRepo is a single-member classic encrypted repo with a classic
// recovery key, pushed; the way an existing install looks.
type classicRepo struct {
	cfg    config.Config
	origin string
	repo   *gitwt.Repo
	idPath string
	recCl  string
}

func newClassicRepo(t *testing.T) classicRepo {
	t.Helper()
	cfg, origin := initFixture(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv(routesEnv, "")
	ms, mm := testenv.Binaries(t)
	cfg.MergeDriverPath = mm
	_, rec := newTestIdentity(t, "rec")
	cfg.RecoveryRecipients = []string{rec}
	cfg.HostLabel = ""
	idp := filepath.Join(t.TempDir(), "k", "identity.txt")
	enc := &encryptSpec{IdentityPath: idp, MemsyncExe: ms, Recipients: []string{rec + " # recovery"}}
	if err := bootstrapWith(cfg, false, nil, enc); err != nil {
		t.Fatal(err)
	}
	r := gitwt.New(cfg.SyncDir)
	if agecrypt.SchemeOf(r) != agecrypt.SchemeClassic {
		t.Fatal("fixture must be classic")
	}
	return classicRepo{cfg: cfg, origin: origin, repo: r, idPath: idp, recCl: rec}
}

func (c classicRepo) state(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for _, args := range [][]string{{"for-each-ref"}, {"ls-files", "-s"}, {"status", "--porcelain"}, {"rev-parse", "HEAD"}} {
		o, _ := c.repo.Run(args...)
		b.WriteString(o)
	}
	for _, f := range []string{".age-recipients", "config.json"} {
		d, _ := os.ReadFile(filepath.Join(c.cfg.SyncDir, f))
		b.Write(d)
	}
	ents, _ := os.ReadDir(filepath.Dir(c.idPath))
	for _, e := range ents {
		b.WriteString(e.Name() + "\n")
	}
	o, _ := gitwt.New(c.origin).Run("rev-parse", "main")
	b.WriteString(o)
	return b.String()
}

func rekey(t *testing.T, c classicRepo, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := recipientsCmd(&out, &errb, append([]string{"rekey-pq", "--config", c.cfg.Path()}, args...))
	return code, out.String(), errb.String()
}

func TestRekeyPQ_DryRunChangesNothing(t *testing.T) {
	c := newClassicRepo(t)
	_, newRec := newPQKey(t, "newrec")
	before := c.state(t)
	for _, extra := range [][]string{nil, {"--fresh-root"}} {
		args := append([]string{"--recovery-recipient", newRec, "--host", "vm-1"}, extra...)
		code, out, errb := rekey(t, c, args...)
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errb)
		}
		for _, want := range []string{"dry run", "recipients now: 2", "after: 2", "files to re-encrypt: "} {
			if !strings.Contains(out, want) {
				t.Errorf("plan lacks %q:\n%s", want, out)
			}
		}
		if len(extra) > 0 && !strings.Contains(out, "REPLACED") {
			t.Errorf("a fresh root must say history is replaced:\n%s", out)
		}
		if strings.Contains(out+errb, newRec) {
			t.Error("the plan prints counts, not keys")
		}
	}
	if after := c.state(t); after != before {
		t.Error("a dry run changed the repo, config, refs or identity files")
	}
}

func TestRekeyPQ_ApplyFreshRootProducesOneHybridCommit(t *testing.T) {
	c := newClassicRepo(t)
	oldTip, _ := gitwt.New(c.origin).Run("rev-parse", "main")
	oldTip = strings.TrimSpace(oldTip)
	oldSecret := secretOf(t, c.idPath)
	_, newRec := newPQKey(t, "newrec")
	code, out, errb := rekey(t, c, "--recovery-recipient", newRec, "--host", "vm-1", "--fresh-root", "--apply")
	if code != 0 {
		t.Fatalf("exit %d: %s%s", code, out, errb)
	}
	origin := gitwt.New(c.origin)
	if n, _ := origin.Run("rev-list", "--count", "main"); strings.TrimSpace(n) != "1" {
		t.Fatalf("origin/main has %s commits", n)
	}
	if par, _ := origin.Run("rev-list", "--parents", "-n1", "main"); len(strings.Fields(par)) != 1 {
		t.Errorf("the root commit has parents: %s", par)
	}
	if cur, _ := origin.Run("rev-parse", "main"); strings.TrimSpace(cur) == oldTip {
		t.Error("origin did not move")
	}
	files, _ := origin.Run("ls-tree", "-r", "--name-only", "main")
	n := 0
	for _, f := range strings.Fields(files) {
		if agecrypt.Exempt(f) {
			continue
		}
		b, _ := origin.Run("cat-file", "blob", "main:"+f)
		if !agecrypt.IsHybridCiphertext([]byte(b)) {
			t.Errorf("%s: not hybrid ciphertext on origin", f)
		}
		n++
	}
	if n == 0 {
		t.Fatal("nothing re-encrypted")
	}
	// recipients: exactly this host (labelled) and the recovery key
	rcp, _ := origin.Run("cat-file", "blob", "main:.age-recipients")
	var keyLines []string
	for _, l := range strings.Split(rcp, "\n") {
		if l != "" && !strings.HasPrefix(l, "#") {
			keyLines = append(keyLines, l)
		}
	}
	newOwn, _ := agecrypt.IdentityRecipient(c.idPath)
	cfgAfter, _ := config.Load(c.cfg.Path())
	_ = newOwn
	if len(keyLines) != 2 || !strings.HasSuffix(keyLines[0], " # host:vm-1") || keyLines[1] != newRec+" # recovery" || !strings.HasPrefix(keyLines[0], "age1pq1") {
		t.Errorf("recipients: %d lines", len(keyLines))
	}
	// local state
	if agecrypt.SchemeOf(c.repo) != agecrypt.SchemePQ || cfgAfter.EncryptionScheme != "pq" || cfgAfter.HostLabel != "vm-1" ||
		len(cfgAfter.RecoveryRecipients) != 1 || cfgAfter.RecoveryRecipients[0] != newRec {
		t.Errorf("config: %+v", cfgAfter)
	}
	if u, _ := agecrypt.Unreviewed(c.repo); u {
		t.Error("the new set must be pinned")
	}
	if bk, err := c.repo.Run("rev-parse", agecrypt.BackupRef); err != nil || strings.TrimSpace(bk) != oldTip {
		t.Errorf("backup ref must keep the old tip: %v %q", err, bk)
	}
	if !strings.Contains(out, "rollback point") || !strings.Contains(out, "--drop-backup") {
		t.Errorf("the backup must be announced:\n%s", out)
	}
	if _, err := os.Stat(c.idPath + ".classic-retired"); err != nil {
		t.Errorf("old identity must be kept as .classic-retired: %v", err)
	}
	if _, err := os.Stat(c.idPath); err == nil {
		t.Error("the old identity must no longer sit at its old name")
	}
	if strings.Contains(out+errb, oldSecret) || strings.Contains(out+errb, "AGE-SECRET-KEY") {
		t.Error("a secret was printed")
	}
	// the work-tree is intact and clean, and the daemon can keep syncing as pq
	if d, _ := c.repo.Run("status", "--porcelain"); strings.TrimSpace(d) != "" {
		t.Errorf("dirty after rekey: %s", d)
	}
	if err := agecrypt.CheckKeyed(c.repo); err != nil {
		t.Fatalf("keyed after rekey: %v", err)
	}
	cfg2 := cfgAfter
	mem := filepath.Join(cfg2.ClaudeProjectsDir, "p1", "memory", "new.md")
	os.WriteFile(mem, []byte("a new note\n"), 0o600)
	mirror := filepath.Join(cfg2.SyncDir, "projects", "p1", "memory")
	os.MkdirAll(mirror, 0o700)
	os.WriteFile(filepath.Join(mirror, "new.md"), []byte("a new note\n"), 0o600)
	if ok, err := syncOnceForTest(cfg2); err != nil || !ok {
		t.Fatalf("commit after rekey: %v %v", ok, err)
	}
	b, _ := c.repo.Run("cat-file", "blob", "HEAD:projects/p1/memory/new.md")
	if !agecrypt.IsHybridCiphertext([]byte(b)) {
		t.Error("new commits are hybrid")
	}
	if err := agecrypt.PushGuard(c.repo, "main"); err != nil {
		t.Errorf("guard: %v", err)
	}
	// a second run (even with other flags) changes nothing and says it is done
	if code, o, e := rekey(t, c, "--recovery-recipient", newRec, "--host", "vm-1", "--apply"); code != 0 || !strings.Contains(o, "already done") {
		t.Errorf("a rerun of a finished rekey must report \"already done\": %d %s%s", code, o, e)
	}
	// drop the backup
	var o2, e2 bytes.Buffer
	if code := recipientsCmd(&o2, &e2, []string{"rekey-pq", "--config", c.cfg.Path(), "--drop-backup"}); code != 0 {
		t.Fatalf("drop-backup: %s", e2.String())
	}
	if _, err := c.repo.Run("rev-parse", "--verify", "--quiet", agecrypt.BackupRef); err == nil {
		t.Error("backup ref must be gone")
	}
}

func TestRekeyPQ_ApplyWithoutFreshRootAddsOneCommitOnTop(t *testing.T) {
	c := newClassicRepo(t)
	_, newRec := newPQKey(t, "newrec")
	origin := gitwt.New(c.origin)
	before, _ := origin.Run("rev-list", "--count", "main")
	code, out, errb := rekey(t, c, "--recovery-recipient", newRec, "--host", "vm-1", "--apply")
	if code != 0 {
		t.Fatalf("exit %d: %s%s", code, out, errb)
	}
	var b0, b1 int
	_, _ = b0, b1
	after, _ := origin.Run("rev-list", "--count", "main")
	if strings.TrimSpace(after) == strings.TrimSpace(before) {
		t.Error("expected one more commit")
	}
	files, _ := origin.Run("ls-tree", "-r", "--name-only", "main")
	for _, f := range strings.Fields(files) {
		if agecrypt.Exempt(f) {
			continue
		}
		b, _ := origin.Run("cat-file", "blob", "main:"+f)
		if !agecrypt.IsHybridCiphertext([]byte(b)) {
			t.Errorf("%s: not hybrid at the tip", f)
		}
	}
}

func TestRekeyPQ_RefusesAnotherMember(t *testing.T) {
	c := newClassicRepo(t)
	_, other := newTestIdentity(t, "other")
	if err := agecrypt.AddRecipient(c.repo, other); err != nil {
		t.Fatal(err)
	}
	c.repo.Run("push", "origin", "main")
	_, newRec := newPQKey(t, "newrec")
	before := c.state(t)
	for _, extra := range [][]string{nil, {"--apply"}, {"--apply", "--fresh-root"}} {
		code, _, errb := rekey(t, c, append([]string{"--recovery-recipient", newRec, "--host", "vm-1"}, extra...)...)
		if code == 0 || !strings.Contains(errb, "another member") {
			t.Errorf("%v: exit %d: %s", extra, code, errb)
		}
	}
	if c.state(t) != before {
		t.Error("a refused rekey changed something")
	}
}

func TestRekeyPQ_RefusesDirtyUnpushedAndBadKeys(t *testing.T) {
	c := newClassicRepo(t)
	_, newRec := newPQKey(t, "newrec")
	_, classicRec := newTestIdentity(t, "x")
	good := []string{"--recovery-recipient", newRec, "--host", "vm-1", "--apply"}
	if code, _, e := rekey(t, c, "--recovery-recipient", classicRec, "--host", "vm-1", "--apply"); code == 0 || !strings.Contains(e, "age1pq1") {
		t.Errorf("classic recovery key: %d %s", code, e)
	}
	if code, _, _ := rekey(t, c, "--recovery-recipient", newRec, "--host", "Bad_Label", "--apply"); code == 0 {
		t.Error("bad host label")
	}
	if code, _, _ := rekey(t, c, "--recovery-recipient", newRec, "--apply"); code != 2 {
		t.Error("--host is required")
	}
	// dirty
	f := filepath.Join(c.cfg.SyncDir, "projects", "p1", "memory", "ok.md")
	orig, _ := os.ReadFile(f)
	os.WriteFile(f, []byte("changed\n"), 0o600)
	snap := c.state(t)
	if code, _, e := rekey(t, c, good...); code == 0 || !strings.Contains(e, "uncommitted") {
		t.Errorf("dirty: %d %s", code, e)
	}
	if c.state(t) != snap {
		t.Error("dirty refusal changed state")
	}
	os.WriteFile(f, orig, 0o600)
	// unpushed commit
	c.repo.Run("add", "-A")
	os.WriteFile(f, []byte("committed locally\n"), 0o600)
	c.repo.Run("add", "-A")
	if _, err := c.repo.Commit("local"); err != nil {
		t.Fatal(err)
	}
	snap = c.state(t)
	if code, _, e := rekey(t, c, good...); code == 0 || !strings.Contains(e, "origin/main") {
		t.Errorf("unpushed: %d %s", code, e)
	}
	if c.state(t) != snap {
		t.Error("unpushed refusal changed state")
	}
	// the daemon holds the lock
	c.repo.Run("push", "origin", "main")
}

func TestRekeyPQ_ClassicModeCommandsUnchanged(t *testing.T) {
	c := newClassicRepo(t)
	_, other := newTestIdentity(t, "other")
	var out, errb bytes.Buffer
	if code := recipientsCmd(&out, &errb, []string{"add", other, "--host", "vm-2", "--config", c.cfg.Path()}); code != 0 {
		t.Fatalf("classic add: %s", errb.String())
	}
	_, pq := newPQKey(t, "p")
	errb.Reset()
	if code := recipientsCmd(&out, &errb, []string{"add", pq, "--config", c.cfg.Path()}); code == 0 {
		t.Error("hybrid add into a classic repo must be refused")
	}
}

func TestPQ_RecipientsAddRefusesClassicUnderPQ(t *testing.T) {
	cfg, _ := initFixture(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv(routesEnv, "")
	ms, mm := testenv.Binaries(t)
	cfg.MergeDriverPath = mm
	enc := &encryptSpec{IdentityPath: filepath.Join(t.TempDir(), "id"), MemsyncExe: ms, Scheme: agecrypt.SchemePQ}
	if err := bootstrapWith(cfg, false, nil, enc); err != nil {
		t.Fatal(err)
	}
	_, cl := newTestIdentity(t, "c")
	_, pq := newPQKey(t, "p")
	var out, errb bytes.Buffer
	if code := recipientsCmd(&out, &errb, []string{"add", cl, "--config", cfg.Path()}); code == 0 || !strings.Contains(errb.String(), "post-quantum") {
		t.Errorf("classic add under pq: %d %s", code, errb.String())
	}
	errb.Reset()
	if code := recipientsCmd(&out, &errb, []string{"add", pq, "--host", "vm-9", "--config", cfg.Path()}); code != 0 {
		t.Errorf("hybrid add under pq: %s", errb.String())
	}
	errb.Reset()
	if code := recipientsCmd(&out, &errb, []string{"label", "self", "--host", "vm-1", "--config", cfg.Path()}); code != 0 {
		t.Errorf("label self with a hybrid key: %s", errb.String())
	}
	_, rec := newPQKey(t, "rec")
	errb.Reset()
	if code := recipientsCmd(&out, &errb, []string{"add", rec, "--config", cfg.Path()}); code != 0 {
		t.Fatalf("add recovery: %s", errb.String())
	}
	errb.Reset()
	if code := recipientsCmd(&out, &errb, []string{"label", "recovery", rec, "--config", cfg.Path()}); code != 0 {
		t.Errorf("label recovery with a hybrid key: %s", errb.String())
	}
}
