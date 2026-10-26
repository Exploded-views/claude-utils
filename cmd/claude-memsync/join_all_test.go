package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/lock"
)

// joinAllHost builds a member host with one encrypted repo per name.
func joinAllHost(t *testing.T, names ...string) map[string]config.Config {
	t.Helper()
	cfgs := routedHost(t, names, names[0], nil)
	out := map[string]config.Config{}
	for _, n := range names {
		out[n] = encryptRouted(t, cfgs[n])
	}
	return out
}

func newAgeRecipient(t *testing.T) string {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id.Recipient().String()
}

// addJoinRequest files a request for label (as the joining host's join request
// would have) in cfg's repo and pushes it to origin. It returns the
// fingerprint the joining host would have printed.
func addJoinRequest(t *testing.T, cfg config.Config, label, recipient string) string {
	t.Helper()
	j := agecrypt.Join{Label: label, Recipient: recipient, Fingerprint: agecrypt.JoinFingerprint(recipient), Requested: time.Now().UTC().Truncate(time.Second)}
	addRawJoinFile(t, cfg, label, j.Format(), true)
	return j.Fingerprint
}

// addRawJoinFile commits joins/<label>.txt with the given content and pushes it
// (through the join-request guard when guard is set, which refuses a bad blob).
func addRawJoinFile(t *testing.T, cfg config.Config, label, content string, guard bool) {
	t.Helper()
	repo := gitwt.New(cfg.SyncDir)
	if err := agecrypt.WriteInfoAttributes(repo); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(cfg.SyncDir, filepath.FromSlash(agecrypt.JoinPath(label)))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Run("add", "-f", agecrypt.JoinPath(label)); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("memsync: join request"); err != nil {
		t.Fatal(err)
	}
	if guard {
		if err := agecrypt.PushGuardJoinRequest(repo, "main"); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.Push("main"); err != nil {
		t.Fatal(err)
	}
}

// tamperOrigin pushes, straight to cfg's origin and behind this clone's back, a
// request under label for another key. This clone's HEAD keeps what it had.
func tamperOrigin(t *testing.T, cfg config.Config, label, recipient string) {
	t.Helper()
	j := agecrypt.Join{Label: label, Recipient: recipient, Fingerprint: agecrypt.JoinFingerprint(recipient), Requested: time.Now().UTC().Truncate(time.Second)}
	dir := filepath.Join(t.TempDir(), "tamper")
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("clone", "-q", cfg.RemoteURL, dir)
	if err := os.MkdirAll(filepath.Join(dir, "joins"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(agecrypt.JoinPath(label))), []byte(j.Format()), 0o600); err != nil {
		t.Fatal(err)
	}
	run("-C", dir, "add", "-f", "joins")
	run("-C", dir, "commit", "-q", "-m", "other key under the same label")
	run("-C", dir, "push", "-q", "origin", "HEAD:main")
}

// runningDaemon models the service holding every dir's lock until stopped.
func runningDaemon(t *testing.T, dirs ...string) *fakeSvc {
	t.Helper()
	f := &fakeSvc{status: "running"}
	installFakeSvc(t, f)
	var held []*lock.Lock
	take := func() {
		for _, d := range dirs {
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
	return f
}

func joinRequestHere(t *testing.T, cfg config.Config, label string) bool {
	t.Helper()
	es, err := agecrypt.ListJoins(gitwt.New(cfg.SyncDir))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range es {
		if e.Join.Label == label {
			return true
		}
	}
	return false
}

func TestJoinAcceptAll_AcceptsEveryMatchingRepoAndRestartsOnce(t *testing.T) {
	cfgs := joinAllHost(t, "personal", "projects")
	rec := newAgeRecipient(t)
	fp := addJoinRequest(t, cfgs["personal"], "new-host", rec)
	addJoinRequest(t, cfgs["projects"], "new-host", rec)
	f := runningDaemon(t, cfgs["personal"].SyncDir, cfgs["projects"].SyncDir)

	code, out, errs := runJoinCmd("accept", "new-host", "--all", "--allow-unlabelled", "--fingerprint", fp)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	for _, n := range []string{"personal", "projects"} {
		if !strings.Contains(out, n+": accepted") {
			t.Errorf("no accepted line for %s:\n%s", n, out)
		}
		if joinRequestHere(t, cfgs[n], "new-host") {
			t.Errorf("%s: the request is still there", n)
		}
		ls, _ := agecrypt.ListRecipients(cfgs[n].SyncDir)
		if !strings.Contains(strings.Join(ls, "\n"), rec) {
			t.Errorf("%s: the key was not added", n)
		}
	}
	if f.did() != "stop,start" {
		t.Errorf("service calls = %q, want one stop and one start", f.did())
	}
}

func TestJoinAcceptAll_AMismatchRefusesEveryRepo(t *testing.T) {
	cfgs := joinAllHost(t, "personal", "projects")
	rec := newAgeRecipient(t)
	fp := addJoinRequest(t, cfgs["personal"], "new-host", rec)
	addJoinRequest(t, cfgs["projects"], "new-host", newAgeRecipient(t)) // a second key under the same label
	before := map[string]string{}
	for n, c := range cfgs {
		before[n] = headRev(t, c.SyncDir)
	}
	f := runningDaemon(t, cfgs["personal"].SyncDir, cfgs["projects"].SyncDir)

	code, out, errs := runJoinCmd("accept", "new-host", "--all", "--allow-unlabelled", "--fingerprint", fp)
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s%s", code, out, errs)
	}
	if !strings.Contains(out, "projects: fingerprint mismatch") {
		t.Errorf("the mismatching repo is not named:\n%s", out)
	}
	for n, c := range cfgs {
		if headRev(t, c.SyncDir) != before[n] {
			t.Errorf("%s was committed to", n)
		}
	}
	if strings.Contains(out, "accepted") && !strings.Contains(out, "not accepted") {
		t.Errorf("something was reported accepted:\n%s", out)
	}
	if f.did() != "" {
		t.Errorf("the service was touched: %q", f.did())
	}
}

func TestJoinAcceptAll_RekeyBlockedRepoIsSkippedAndUntouched(t *testing.T) {
	cfgs := joinAllHost(t, "personal", "projects")
	rec := newAgeRecipient(t)
	fp := addJoinRequest(t, cfgs["personal"], "new-host", rec)
	addJoinRequest(t, cfgs["projects"], "new-host", rec)
	bad := cfgs["projects"]
	if err := os.WriteFile(filepath.Join(bad.SyncDir, ".git", agecrypt.RekeyStateFile), []byte("{garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, blocked := agecrypt.RekeyBlocksSync(gitwt.New(bad.SyncDir)); !blocked {
		t.Fatal("setup: the repo must be blocked")
	}
	headBefore := headRev(t, bad.SyncDir)
	f := runningDaemon(t, cfgs["personal"].SyncDir, bad.SyncDir)

	code, out, errs := runJoinCmd("accept", "new-host", "--all", "--allow-unlabelled", "--fingerprint", fp)
	if code != 1 {
		t.Fatalf("exit %d, want 1 (a request was left unanswered)\n%s%s", code, out, errs)
	}
	if !strings.Contains(out, "projects: skipped (rekey-blocked") {
		t.Errorf("the blocked repo is not reported as skipped:\n%s", out)
	}
	if !strings.Contains(out, "personal: accepted") {
		t.Errorf("the healthy repo was not accepted:\n%s", out)
	}
	if headRev(t, bad.SyncDir) != headBefore {
		t.Error("the blocked repo was committed to")
	}
	if f.did() != "stop,start" {
		t.Errorf("service calls = %q", f.did())
	}
}

func TestJoinAcceptAll_NoRequestsAnywhere(t *testing.T) {
	cfgs := joinAllHost(t, "personal", "projects")
	f := runningDaemon(t, cfgs["personal"].SyncDir, cfgs["projects"].SyncDir)
	code, out, errs := runJoinCmd("accept", "new-host", "--all", "--allow-unlabelled", "--fingerprint", "0123456789abcdef0123456789abcdef")
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s%s", code, out, errs)
	}
	if !strings.Contains(out+errs, "no join request from new-host in any repo") {
		t.Errorf("no clear message:\n%s%s", out, errs)
	}
	if f.did() != "" {
		t.Errorf("the service was touched: %q", f.did())
	}
}

func TestJoinAcceptAll_NeedsFingerprintAndNoRepoFlag(t *testing.T) {
	joinAllHost(t, "personal", "projects")
	code, _, errs := runJoinCmd("accept", "new-host", "--all")
	if code != 2 || !strings.Contains(errs, "refusing to accept without --fingerprint") {
		t.Errorf("no fingerprint: exit %d, %q", code, errs)
	}
	code, _, errs = runJoinCmd("accept", "new-host", "--all", "--repo", "projects", "--fingerprint", "ab")
	if code != 2 || !strings.Contains(errs, "--all does not go with --repo or --config") {
		t.Errorf("--all with --repo: exit %d, %q", code, errs)
	}
	for _, act := range []string{"reject", "list"} {
		code, _, errs = runJoinCmd(act, "new-host", "--all", "--fingerprint", "ab")
		if code != 2 || !strings.Contains(errs, "--fingerprint") {
			t.Errorf("%s --all --fingerprint: exit %d, %q", act, code, errs)
		}
	}
}

func TestJoinRejectAll_RemovesTheRequestEverywhereOnce(t *testing.T) {
	cfgs := joinAllHost(t, "personal", "projects")
	rec := newAgeRecipient(t)
	addJoinRequest(t, cfgs["personal"], "new-host", rec)
	addJoinRequest(t, cfgs["projects"], "new-host", newAgeRecipient(t))
	f := runningDaemon(t, cfgs["personal"].SyncDir, cfgs["projects"].SyncDir)
	code, out, errs := runJoinCmd("reject", "new-host", "--all")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	for _, n := range []string{"personal", "projects"} {
		if !strings.Contains(out, n+": rejected") || joinRequestHere(t, cfgs[n], "new-host") {
			t.Errorf("%s: not rejected:\n%s", n, out)
		}
	}
	if f.did() != "stop,start" {
		t.Errorf("service calls = %q", f.did())
	}
}

func TestJoinListAll_OneTableAcrossRepos(t *testing.T) {
	cfgs := joinAllHost(t, "personal", "projects")
	addJoinRequest(t, cfgs["personal"], "host-a", newAgeRecipient(t))
	addJoinRequest(t, cfgs["projects"], "host-b", newAgeRecipient(t))
	code, out, errs := runJoinCmd("list", "--all")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	var rows []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		rows = append(rows, l)
	}
	if len(rows) != 2 || !strings.HasPrefix(rows[0], "personal\thost-a\t") || !strings.HasPrefix(rows[1], "projects\thost-b\t") {
		t.Errorf("want one row per request, repo first:\n%s", out)
	}
}

// ---- review round: the mismatch check must see origin, and must be re-made under the lock ----

func TestJoinAcceptAll_OriginHoldsAnotherKeyUnderTheLabel(t *testing.T) {
	cfgs := joinAllHost(t, "personal", "projects")
	rec := newAgeRecipient(t)
	fp := addJoinRequest(t, cfgs["personal"], "new-host", rec)
	addJoinRequest(t, cfgs["projects"], "new-host", rec)
	tamperOrigin(t, cfgs["projects"], "new-host", newAgeRecipient(t)) // HEAD here still has the honest one
	before := map[string]string{}
	for n, c := range cfgs {
		before[n] = headRev(t, c.SyncDir)
	}
	f := runningDaemon(t, cfgs["personal"].SyncDir, cfgs["projects"].SyncDir)
	code, out, errs := runJoinCmd("accept", "new-host", "--all", "--allow-unlabelled", "--fingerprint", fp)
	if code != 1 || !strings.Contains(out, "projects: fingerprint mismatch") {
		t.Fatalf("exit %d, want 1 naming projects\n%s%s", code, out, errs)
	}
	for n, c := range cfgs {
		if headRev(t, c.SyncDir) != before[n] {
			t.Errorf("%s was committed to", n)
		}
	}
	if f.did() != "" {
		t.Errorf("the service was touched: %q", f.did())
	}
}

// The other key appears on origin after the first look, while the service is
// being stopped: the view under the lock decides.
func TestJoinAcceptAll_AMismatchThatAppearsAfterTheFirstLookRefusesEveryRepo(t *testing.T) {
	cfgs := joinAllHost(t, "personal", "projects")
	rec := newAgeRecipient(t)
	fp := addJoinRequest(t, cfgs["personal"], "new-host", rec)
	addJoinRequest(t, cfgs["projects"], "new-host", rec)
	before := map[string]string{}
	for n, c := range cfgs {
		before[n] = headRev(t, c.SyncDir)
	}
	f := runningDaemon(t, cfgs["personal"].SyncDir, cfgs["projects"].SyncDir)
	stop := f.onStop
	f.onStop = func() {
		stop()
		tamperOrigin(t, cfgs["projects"], "new-host", newAgeRecipient(t))
	}
	code, out, errs := runJoinCmd("accept", "new-host", "--all", "--allow-unlabelled", "--fingerprint", fp)
	if code != 1 || !strings.Contains(out, "projects: fingerprint mismatch") {
		t.Fatalf("exit %d, want 1 naming projects\n%s%s", code, out, errs)
	}
	// personal is untouched; projects only fast-forwarded to origin's tip, and
	// neither repo gained the key.
	if headRev(t, cfgs["personal"].SyncDir) != before["personal"] {
		t.Error("personal was committed to")
	}
	for n, c := range cfgs {
		if ls, _ := agecrypt.ListRecipients(c.SyncDir); strings.Contains(strings.Join(ls, "\n"), rec) {
			t.Errorf("%s: the key was added", n)
		}
	}
	if f.did() != "stop,start" {
		t.Errorf("service calls = %q", f.did())
	}
}

func TestJoinAcceptAll_AMalformedRequestUnderTheLabelRefusesEveryRepo(t *testing.T) {
	cfgs := joinAllHost(t, "personal", "projects")
	rec := newAgeRecipient(t)
	fp := addJoinRequest(t, cfgs["personal"], "new-host", rec)
	addRawJoinFile(t, cfgs["projects"], "new-host", "this is not a request\n", false)
	before := headRev(t, cfgs["personal"].SyncDir)
	runningDaemon(t, cfgs["personal"].SyncDir, cfgs["projects"].SyncDir)
	code, out, errs := runJoinCmd("accept", "new-host", "--all", "--allow-unlabelled", "--fingerprint", fp)
	if code != 1 || !strings.Contains(out, "projects: malformed") {
		t.Fatalf("exit %d, want 1 naming projects as malformed\n%s%s", code, out, errs)
	}
	if headRev(t, cfgs["personal"].SyncDir) != before {
		t.Error("personal was accepted although another repo holds a malformed request")
	}
}

// ---- interrupts ----

func TestJoinAcceptAll_AnInterruptStopsBetweenReposAndReportsWhatWasDone(t *testing.T) {
	cfgs := joinAllHost(t, "personal", "projects")
	rec := newAgeRecipient(t)
	fp := addJoinRequest(t, cfgs["personal"], "new-host", rec)
	addJoinRequest(t, cfgs["projects"], "new-host", rec)
	headProjects := headRev(t, cfgs["projects"].SyncDir)
	f := runningDaemon(t, cfgs["personal"].SyncDir, cfgs["projects"].SyncDir)
	sig := make(chan os.Signal, 2)
	notifySignals = func() (<-chan os.Signal, func()) { return sig, func() {} }
	// The first push (personal's) is where the interrupt arrives.
	joinGuardHook = func(string) {
		joinGuardHook = nil
		sig <- syscall.SIGINT
		for i := 0; i < 400 && !syncInterrupted.Load(); i++ {
			time.Sleep(5 * time.Millisecond)
		}
	}
	t.Cleanup(func() { joinGuardHook = nil })
	code, out, errs := runJoinCmd("accept", "new-host", "--all", "--allow-unlabelled", "--fingerprint", fp)
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s%s", code, out, errs)
	}
	if !strings.Contains(out, "personal: accepted and pushed") {
		t.Errorf("the repo that was accepted is not reported as such:\n%s", out)
	}
	if !strings.Contains(out, "projects: not run (interrupted)") {
		t.Errorf("the repo that never ran is not reported:\n%s", out)
	}
	if !strings.Contains(out, "recipients accept --yes") {
		t.Errorf("no follow-up hint for the accepted repo:\n%s", out)
	}
	if headRev(t, cfgs["projects"].SyncDir) != headProjects {
		t.Error("projects was changed after the interrupt")
	}
	if f.did() != "stop,start" {
		t.Errorf("service calls = %q", f.did())
	}
}

// ---- coverage of the skips and the lock failure ----

func TestJoinAcceptAll_ReadOnlyRepoIsSkippedAndUntouched(t *testing.T) {
	cfgs := joinAllHost(t, "personal", "projects")
	rec := newAgeRecipient(t)
	fp := addJoinRequest(t, cfgs["personal"], "new-host", rec)
	addJoinRequest(t, cfgs["projects"], "new-host", rec)
	ro := cfgs["projects"]
	ro.ReadOnly = true
	if err := config.Save(ro); err != nil {
		t.Fatal(err)
	}
	before := headRev(t, ro.SyncDir)
	runningDaemon(t, cfgs["personal"].SyncDir)
	code, out, errs := runJoinCmd("accept", "new-host", "--all", "--allow-unlabelled", "--fingerprint", fp)
	if code != 1 || !strings.Contains(out, "projects: skipped (read-only") || !strings.Contains(out, "personal: accepted") {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	if headRev(t, ro.SyncDir) != before {
		t.Error("the read-only repo was committed to")
	}
}

func TestJoinAcceptAll_UnencryptedAndNotACloneAreSkipped(t *testing.T) {
	cfgs := routedHost(t, []string{"personal", "plain", "broken"}, "personal", nil)
	good := encryptRouted(t, cfgs["personal"])
	rec := newAgeRecipient(t)
	fp := addJoinRequest(t, good, "new-host", rec)
	if err := os.RemoveAll(filepath.Join(cfgs["broken"].SyncDir, ".git")); err != nil {
		t.Fatal(err)
	}
	runningDaemon(t, good.SyncDir)
	code, out, errs := runJoinCmd("accept", "new-host", "--all", "--allow-unlabelled", "--fingerprint", fp)
	if code != 0 {
		t.Fatalf("skips that hold no request are not failures: exit %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{"plain: skipped (unencrypted)", "broken: skipped (missing", "personal: accepted"} {
		if !strings.Contains(out, want) {
			t.Errorf("want %q:\n%s", want, out)
		}
	}
}

func TestJoinAcceptAll_LockHeldByAnotherProcessRefusesPlainly(t *testing.T) {
	cfgs := joinAllHost(t, "personal", "projects")
	rec := newAgeRecipient(t)
	fp := addJoinRequest(t, cfgs["personal"], "new-host", rec)
	addJoinRequest(t, cfgs["projects"], "new-host", rec)
	l, err := lock.Acquire(cfgs["projects"].SyncDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Release)
	f := &fakeSvc{status: "stopped"}
	installFakeSvc(t, f)
	before := headRev(t, cfgs["personal"].SyncDir)
	code, out, errs := runJoinCmd("accept", "new-host", "--all", "--allow-unlabelled", "--fingerprint", fp)
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s%s", code, out, errs)
	}
	if strings.Contains(errs, "recipients: join:") {
		t.Errorf("the lock error carries a misleading prefix:\n%s", errs)
	}
	if !strings.Contains(out, "personal: not run") || !strings.Contains(out, "projects: not run") {
		t.Errorf("the repos that did not run are not reported:\n%s", out)
	}
	if headRev(t, cfgs["personal"].SyncDir) != before || f.did() != "" {
		t.Error("something changed although the lock was not taken")
	}
}

func TestJoinAcceptAll_PrintsTheAcceptingHostsKey(t *testing.T) {
	cfgs := joinAllHost(t, "personal", "projects")
	rec := newAgeRecipient(t)
	fp := addJoinRequest(t, cfgs["personal"], "new-host", rec)
	addJoinRequest(t, cfgs["projects"], "new-host", rec)
	runningDaemon(t, cfgs["personal"].SyncDir, cfgs["projects"].SyncDir)
	code, out, errs := runJoinCmd("accept", "new-host", "--all", "--allow-unlabelled", "--fingerprint", fp)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	if n := strings.Count(out, "key fingerprint"); n != 2 {
		t.Errorf("want the accepting host's key line for each accepted repo, got %d:\n%s", n, out)
	}
}

// ---- round 2: a diverged clone, and a fetch that fails, must not hide origin's other key ----

// pushUnrelatedToOrigin moves origin on without touching joins/.
func pushUnrelatedToOrigin(t *testing.T, cfg config.Config) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "other")
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("clone", "-q", cfg.RemoteURL, dir)
	run("-C", dir, "commit", "-q", "--allow-empty", "-m", "origin moves on")
	run("-C", dir, "push", "-q", "origin", "HEAD:main")
}

func localUnpushedCommit(t *testing.T, cfg config.Config) {
	t.Helper()
	if _, err := gitwt.New(cfg.SyncDir).Run("commit", "-q", "--allow-empty", "-m", "local unpushed"); err != nil {
		t.Fatal(err)
	}
}

func keyAdded(t *testing.T, cfg config.Config, rec string) bool {
	t.Helper()
	ls, _ := agecrypt.ListRecipients(cfg.SyncDir)
	return strings.Contains(strings.Join(ls, "\n"), rec)
}

func TestJoinAcceptAll_ADivergedCloneStillShowsOriginsOtherKey(t *testing.T) {
	cfgs := joinAllHost(t, "personal", "projects")
	rec := newAgeRecipient(t)
	fp := addJoinRequest(t, cfgs["personal"], "new-host", rec)
	addJoinRequest(t, cfgs["projects"], "new-host", rec) // the honest request at HEAD
	localUnpushedCommit(t, cfgs["projects"])
	tamperOrigin(t, cfgs["projects"], "new-host", newAgeRecipient(t))
	runningDaemon(t, cfgs["personal"].SyncDir, cfgs["projects"].SyncDir)
	code, out, errs := runJoinCmd("accept", "new-host", "--all", "--allow-unlabelled", "--fingerprint", fp)
	if code != 1 || !strings.Contains(out, "projects: fingerprint mismatch") {
		t.Fatalf("exit %d, want 1 naming projects\n%s%s", code, out, errs)
	}
	if keyAdded(t, cfgs["personal"], rec) {
		t.Error("personal accepted while projects' origin holds another key under the label")
	}
}

func TestJoinAcceptAll_ADivergedReadOnlyCloneWithNoRequestStillShowsOriginsOtherKey(t *testing.T) {
	cfgs := joinAllHost(t, "personal", "projects")
	rec := newAgeRecipient(t)
	fp := addJoinRequest(t, cfgs["personal"], "new-host", rec)
	localUnpushedCommit(t, cfgs["projects"])
	tamperOrigin(t, cfgs["projects"], "new-host", newAgeRecipient(t))
	ro := cfgs["projects"]
	ro.ReadOnly = true
	if err := config.Save(ro); err != nil {
		t.Fatal(err)
	}
	runningDaemon(t, cfgs["personal"].SyncDir)
	code, out, errs := runJoinCmd("accept", "new-host", "--all", "--allow-unlabelled", "--fingerprint", fp)
	if code != 1 || !strings.Contains(out, "projects: fingerprint mismatch") {
		t.Fatalf("exit %d, want 1 naming projects\n%s%s", code, out, errs)
	}
	if keyAdded(t, cfgs["personal"], rec) {
		t.Error("personal accepted while a read-only repo's origin holds another key")
	}
}

// A candidate that cannot be brought up to date under the lock leaves its
// origin's state unknown: nothing is accepted anywhere.
func TestJoinAcceptAll_ACandidateThatCannotSyncRefusesEveryRepo(t *testing.T) {
	cfgs := joinAllHost(t, "personal", "projects")
	rec := newAgeRecipient(t)
	fp := addJoinRequest(t, cfgs["personal"], "new-host", rec)
	addJoinRequest(t, cfgs["projects"], "new-host", rec)
	localUnpushedCommit(t, cfgs["projects"])
	pushUnrelatedToOrigin(t, cfgs["projects"]) // diverged, but no other key
	runningDaemon(t, cfgs["personal"].SyncDir, cfgs["projects"].SyncDir)
	code, out, errs := runJoinCmd("accept", "new-host", "--all", "--allow-unlabelled", "--fingerprint", fp)
	if code != 1 || !strings.Contains(out, "projects: cannot sync") || !strings.Contains(out, "nothing accepted anywhere") {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	if keyAdded(t, cfgs["personal"], rec) {
		t.Error("personal was accepted although projects could not be synced")
	}
}

func TestJoinAcceptAll_AFailedFetchRefusesEveryRepo(t *testing.T) {
	cfgs := joinAllHost(t, "personal", "projects")
	rec := newAgeRecipient(t)
	fp := addJoinRequest(t, cfgs["personal"], "new-host", rec)
	tamperOrigin(t, cfgs["projects"], "new-host", newAgeRecipient(t)) // unseen: the fetch will fail
	if _, err := gitwt.New(cfgs["projects"].SyncDir).Run("remote", "set-url", "origin", filepath.Join(t.TempDir(), "nonexistent.git")); err != nil {
		t.Fatal(err)
	}
	f := runningDaemon(t, cfgs["personal"].SyncDir, cfgs["projects"].SyncDir)
	code, out, errs := runJoinCmd("accept", "new-host", "--all", "--allow-unlabelled", "--fingerprint", fp)
	if code != 1 || !strings.Contains(out, "projects: cannot fetch") || !strings.Contains(out, "nothing accepted anywhere") {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	if keyAdded(t, cfgs["personal"], rec) {
		t.Error("personal accepted although projects' origin could not be read")
	}
	if f.did() != "" {
		t.Errorf("the service was touched: %q", f.did())
	}
}
