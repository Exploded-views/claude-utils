package main

import (
	"os"
	"path/filepath"
	"strings"
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
	repo := gitwt.New(cfg.SyncDir)
	j := agecrypt.Join{Label: label, Recipient: recipient, Fingerprint: agecrypt.JoinFingerprint(recipient), Requested: time.Now().UTC().Truncate(time.Second)}
	if err := agecrypt.WriteInfoAttributes(repo); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(cfg.SyncDir, filepath.FromSlash(agecrypt.JoinPath(label)))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(j.Format()), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Run("add", "-f", agecrypt.JoinPath(label)); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("memsync: join request"); err != nil {
		t.Fatal(err)
	}
	if err := agecrypt.PushGuardJoinRequest(repo, "main"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Push("main"); err != nil {
		t.Fatal(err)
	}
	return j.Fingerprint
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
	if code, _, _ := runJoinCmd("accept", "new-host", "--all"); code != 2 {
		t.Errorf("no fingerprint: exit %d, want 2", code)
	}
	if code, _, _ := runJoinCmd("accept", "new-host", "--all", "--repo", "projects", "--fingerprint", "ab"); code != 2 {
		t.Errorf("--all with --repo: exit %d, want 2", code)
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
