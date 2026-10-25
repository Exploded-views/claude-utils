package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	syncpkg "github.com/MarimerLLC/claude-utils/internal/sync"
	"github.com/MarimerLLC/claude-utils/internal/testenv"
)

type joinHosts struct {
	a, b   config.Config
	ra, rb *gitwt.Repo
	origin string
	idB    string
}

// twoHosts: a is a member of an encrypted repo; b is keyless on it.
func twoHosts(t *testing.T) joinHosts {
	t.Helper()
	cfg, origin := initFixture(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv(routesEnv, "")
	ms, mm := testenv.Binaries(t)
	cfg.MergeDriverPath = mm
	if err := bootstrapWith(cfg, false, nil, &encryptSpec{IdentityPath: filepath.Join(t.TempDir(), "id1"), MemsyncExe: ms}); err != nil {
		t.Fatal(err)
	}
	cfg2 := cfg
	cfg2.SyncDir = filepath.Join(t.TempDir(), "second")
	cfg2.ClaudeProjectsDir = filepath.Join(t.TempDir(), "claude2")
	idB := filepath.Join(t.TempDir(), "id2")
	if err := bootstrapWith(cfg2, false, nil, &encryptSpec{IdentityPath: idB, MemsyncExe: ms}); err != nil {
		t.Fatal(err)
	}
	h := joinHosts{a: cfg, b: cfg2, ra: gitwt.New(cfg.SyncDir), rb: gitwt.New(cfg2.SyncDir), origin: origin, idB: idB}
	if agecrypt.CheckKeyed(h.rb) == nil {
		t.Fatal("b must be keyless")
	}
	return h
}

func runJoinCmd(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := joinCmd(&out, &errb, args)
	return code, out.String(), errb.String()
}

func fpOf(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(l, "fingerprint: "); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func TestJoinCLI_RequestListAcceptStatus(t *testing.T) {
	h := twoHosts(t)
	code, out, errs := runJoinCmd("request", "--config", h.b.Path(), "--label", "new-host")
	if code != 0 {
		t.Fatalf("request: %d %s", code, errs)
	}
	fp := fpOf(out)
	if fp == "" {
		t.Fatalf("request must print the fingerprint: %q", out)
	}
	if !strings.Contains(originFiles(t, h.origin), "joins/new-host.txt") {
		t.Fatalf("request not pushed: %s", originFiles(t, h.origin))
	}
	// b's own view
	if code, out, _ := runJoinCmd("status", "--config", h.b.Path()); code != 0 || !strings.Contains(out, "pending") || !strings.Contains(out, fp) {
		t.Errorf("status on b: %d %q", code, out)
	}
	// idempotent: same fingerprint, no error
	if code, out2, _ := runJoinCmd("request", "--config", h.b.Path(), "--label", "new-host"); code != 0 || fpOf(out2) != fp {
		t.Errorf("repeat request: %d %q", code, out2)
	}

	// member sees it without pulling by hand (list fetches), and status counts it
	code, out, errs = runJoinCmd("list", "--config", h.a.Path())
	if code != 0 || !strings.Contains(out, "new-host") || !strings.Contains(out, fp) {
		t.Fatalf("list: %d %q %s", code, out, errs)
	}
	if _, err := h.ra.Run("pull", "-q", "--rebase", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	if lines := strings.Join(encryptionLines(h.a), "\n"); !strings.Contains(lines, "1 join request(s) pending") {
		t.Errorf("status: %s", lines)
	}

	// accept: no fingerprint / wrong fingerprint refused
	headBefore, _ := h.ra.Run("rev-parse", "HEAD")
	if code, _, _ := runJoinCmd("accept", "new-host", "--config", h.a.Path()); code == 0 {
		t.Fatal("accept without fingerprint must fail")
	}
	if code, _, _ := runJoinCmd("accept", "new-host", "--config", h.a.Path(), "--fingerprint", "0000000000000000"); code == 0 {
		t.Fatal("accept with wrong fingerprint must fail")
	}
	if now, _ := h.ra.Run("rev-parse", "HEAD"); now != headBefore {
		t.Fatal("refused accepts must not commit")
	}
	if code, _, errs := runJoinCmd("accept", "new-host", "--config", h.a.Path(), "--fingerprint", fp); code != 0 {
		t.Fatalf("accept: %d %s", code, errs)
	}
	files := originFiles(t, h.origin)
	if strings.Contains(files, "joins/") {
		t.Errorf("join file must be removed on the remote: %s", files)
	}
	if lines := strings.Join(encryptionLines(h.a), "\n"); strings.Contains(lines, "join request") {
		t.Errorf("no request should be pending: %s", lines)
	}

	// b: sees accepted, pulls, must still run recipients accept --yes
	if code, out, _ := runJoinCmd("status", "--config", h.b.Path()); code != 0 || !strings.Contains(out, "accepted") {
		t.Errorf("status after accept: %d %q", code, out)
	}
	if _, err := h.rb.Run("pull", "-q", "--rebase", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	if u, _ := agecrypt.Unreviewed(h.rb); !u {
		t.Fatal("pinned-set change must need review on b")
	}
	if code, out, _ := runJoinCmd("status", "--config", h.b.Path()); !strings.Contains(out, "recipients accept --yes") {
		t.Errorf("status must point at recipients accept: %d %q", code, out)
	}
	var o, e bytes.Buffer
	if c := recipientsCmd(&o, &e, []string{"accept", "--yes", "--config", h.b.Path()}); c != 0 {
		t.Fatalf("recipients accept: %d %s", c, e.String())
	}
	if err := agecrypt.CheckKeyed(h.rb); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := runJoinCmd("status", "--config", h.b.Path()); !strings.Contains(out, "can decrypt") {
		t.Errorf("final status: %d %q", code, out)
	}
}

func TestJoinCLI_Reject(t *testing.T) {
	h := twoHosts(t)
	if code, _, e := runJoinCmd("request", "--config", h.b.Path(), "--label", "new-host"); code != 0 {
		t.Fatal(e)
	}
	if code, _, e := runJoinCmd("reject", "new-host", "--config", h.a.Path()); code != 0 {
		t.Fatal(e)
	}
	if strings.Contains(originFiles(t, h.origin), "joins/") {
		t.Error("reject must remove the request on the remote")
	}
	if u, _ := agecrypt.Unreviewed(h.ra); u {
		t.Error("reject changes no recipients")
	}
}

func TestJoinCLI_Usage(t *testing.T) {
	h := twoHosts(t)
	for _, args := range [][]string{
		{},
		{"bogus"},
		{"request", "--config", h.b.Path()},
		{"request", "--config", h.b.Path(), "--label", "Bad Label"},
		{"request", "--config", h.b.Path(), "--label", strings.Repeat("a", 40)},
		{"accept", "--config", h.a.Path()},
		{"request", "--repo", "x", "--config", h.b.Path(), "--label", "ok"},
	} {
		if code, _, _ := runJoinCmd(args...); code == 0 {
			t.Errorf("%v must fail", args)
		}
	}
}

func TestJoinCLI_RequestOnMemberSaysSo(t *testing.T) {
	h := twoHosts(t)
	code, out, e := runJoinCmd("request", "--config", h.a.Path(), "--label", "member")
	if code != 0 || !strings.Contains(out, "already a recipient") {
		t.Errorf("%d %q %s", code, out, e)
	}
}

// A secret staged in the real index of the requesting host is never part of
// the join commit, and the join commit holds one path.
func TestJoinCLI_RequestStagesNothingElse(t *testing.T) {
	h := twoHosts(t)
	p := filepath.Join(h.b.SyncDir, "stray.txt")
	os.WriteFile(p, []byte("TOPSECRET\n"), 0600)
	if _, err := h.rb.Run("add", "stray.txt"); err != nil {
		t.Fatal(err)
	}
	if code, _, e := runJoinCmd("request", "--config", h.b.Path(), "--label", "new-host"); code != 0 {
		t.Fatal(e)
	}
	if out, _ := exec.Command("git", "-C", h.origin, "ls-tree", "-r", "--name-only", "main").Output(); strings.Contains(string(out), "stray") {
		t.Error("staged file leaked into the pushed history")
	}
	if staged, _ := h.rb.Run("diff", "--cached", "--name-only"); strings.TrimSpace(staged) != "stray.txt" {
		t.Errorf("index: %q", staged)
	}
}

// The sync cycle of a member never admits anyone: a pulled request stays a
// request and the recipients stay what they were.
func TestJoinCLI_DaemonCycleDoesNotAccept(t *testing.T) {
	h := twoHosts(t)
	if code, _, e := runJoinCmd("request", "--config", h.b.Path(), "--label", "new-host"); code != 0 {
		t.Fatal(e)
	}
	if _, err := h.ra.Run("pull", "-q", "--rebase", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	before, _ := agecrypt.ListRecipients(h.a.SyncDir)
	if _, err := os.Stat(filepath.Join(h.a.SyncDir, "joins", "new-host.txt")); err != nil {
		t.Fatal("request should be in the member's work-tree")
	}
	if _, err := syncOnceForTest(h.a); err != nil {
		t.Fatal(err)
	}
	after, _ := agecrypt.ListRecipients(h.a.SyncDir)
	if strings.Join(before, ",") != strings.Join(after, ",") {
		t.Fatal("a sync cycle changed the recipients")
	}
	if es, _ := agecrypt.ListJoins(h.ra); len(es) != 1 {
		t.Errorf("request must still be pending: %+v", es)
	}
}

func syncOnceForTest(cfg config.Config) (bool, error) {
	return syncpkg.CommitMirror(gitwt.New(cfg.SyncDir), cfg, "cycle")
}

// With a routes file, --repo names the repo; list without --repo covers all.
func TestJoinCLI_RoutedRepoFlag(t *testing.T) {
	h := twoHosts(t)
	write := func(cfg config.Config) {
		p := filepath.Join(t.TempDir(), "routes.json")
		body := `{"repos":{"shared":{"config":"` + filepath.ToSlash(cfg.Path()) + `","kind":"personal"}},"routes":[],"default":"deny","personal":"shared"}`
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(routesEnv, p)
	}
	write(h.b)
	code, out, e := runJoinCmd("request", "--repo", "shared", "--label", "new-host")
	if code != 0 || fpOf(out) == "" {
		t.Fatalf("routed request: %d %q %s", code, out, e)
	}
	if code, _, _ := runJoinCmd("request", "--repo", "nope", "--label", "new-host"); code == 0 {
		t.Error("unknown repo must fail")
	}
	write(h.a)
	code, out, e = runJoinCmd("list")
	if code != 0 || !strings.Contains(out, "repo shared:") || !strings.Contains(out, "new-host") {
		t.Fatalf("routed list: %d %q %s", code, out, e)
	}
}

var groupedFpRE = regexp.MustCompile(`(?m)^fingerprint: ([0-9a-f]{4} ){7}[0-9a-f]{4}$`)

func TestJoinCLI_FingerprintLineFormat(t *testing.T) {
	h := twoHosts(t)
	code, out, e := runJoinCmd("request", "--config", h.b.Path(), "--label", "new-host")
	if code != 0 || !groupedFpRE.MatchString(out) {
		t.Fatalf("want one line `fingerprint: xxxx xxxx xxxx xxxx xxxx xxxx xxxx xxxx`: %d %q %s", code, out, e)
	}
	// the same line on a repeat
	_, out2, _ := runJoinCmd("request", "--config", h.b.Path(), "--label", "new-host")
	if fpOf(out) != fpOf(out2) {
		t.Errorf("repeat prints another fingerprint: %q %q", out, out2)
	}
	// the member accepts with it typed back ungrouped as well
	runJoinCmd("list", "--config", h.a.Path())
	plain := strings.ReplaceAll(fpOf(out), " ", "")
	if code, _, e := runJoinCmd("accept", "new-host", "--config", h.a.Path(), "--fingerprint", plain); code != 0 {
		t.Fatalf("ungrouped fingerprint: %d %s", code, e)
	}
}

func TestJoinCLI_RequestRefusedWhenSyncFails(t *testing.T) {
	h := twoHosts(t)
	h.rb.Run("remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))
	head, _ := h.rb.Run("rev-parse", "HEAD")
	code, _, e := runJoinCmd("request", "--config", h.b.Path(), "--label", "new-host")
	if code == 0 || !strings.Contains(e, "up to date") {
		t.Fatalf("a failed sync must refuse: %d %s", code, e)
	}
	if now, _ := h.rb.Run("rev-parse", "HEAD"); now != head {
		t.Error("nothing may be committed")
	}
}

func TestJoinCLI_RequestRefusedWhenLocalDiffersFromOrigin(t *testing.T) {
	h := twoHosts(t)
	// a local commit on top of origin's tip, as a stray earlier cycle might leave
	oid, _ := h.rb.RunStdin([]byte("x\n"), "hash-object", "-w", "--no-filters", "--stdin")
	h.rb.Run("update-index", "--add", "--cacheinfo", "100644,"+strings.TrimSpace(oid)+",stray/file.txt")
	if _, err := h.rb.Run("commit", "-q", "-m", "local"); err != nil {
		t.Fatal(err)
	}
	code, _, e := runJoinCmd("request", "--config", h.b.Path(), "--label", "new-host")
	if code == 0 {
		t.Fatalf("must refuse: %s", e)
	}
	if strings.Contains(originFiles(t, h.origin), "joins/") || strings.Contains(originFiles(t, h.origin), "stray") {
		t.Error("nothing may be pushed")
	}
}

func TestJoinCLI_StatusAfterAcceptShowsRecipientSetAndWarns(t *testing.T) {
	h := twoHosts(t)
	_, out, _ := runJoinCmd("request", "--config", h.b.Path(), "--label", "new-host")
	fp := fpOf(out)
	if code, _, e := runJoinCmd("accept", "new-host", "--config", h.a.Path(), "--fingerprint", fp); code != 0 {
		t.Fatal(e)
	}
	if _, err := h.rb.Run("pull", "-q", "--rebase", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	code, st, _ := runJoinCmd("status", "--config", h.b.Path())
	if code != 0 || !strings.Contains(st, "accepted") || !strings.Contains(st, "blindly") {
		t.Fatalf("status: %d %q", code, st)
	}
	rcptB, _ := agecrypt.IdentityRecipient(h.idB)
	if !strings.Contains(st, agecrypt.GroupFingerprint(agecrypt.JoinFingerprint(rcptB))) || !strings.Contains(st, rcptB) {
		t.Errorf("must list this host's key with its fingerprint:\n%s", st)
	}
	if n := strings.Count(st, "recipient: "); n != 2 {
		t.Errorf("want both keys of the pending set, got %d:\n%s", n, st)
	}
	if !strings.Contains(st, "this host") || !strings.Contains(st, "new") {
		t.Errorf("tags missing:\n%s", st)
	}
}

func TestJoinCLI_OnlyRequestUsesTheRequestGuard(t *testing.T) {
	var kinds []string
	joinGuardHook = func(k string) { kinds = append(kinds, k) }
	t.Cleanup(func() { joinGuardHook = nil })
	h := twoHosts(t)
	_, out, _ := runJoinCmd("request", "--config", h.b.Path(), "--label", "new-host")
	if strings.Join(kinds, ",") != "request" {
		t.Fatalf("request: %v", kinds)
	}
	kinds = nil
	if code, _, e := runJoinCmd("accept", "new-host", "--config", h.a.Path(), "--fingerprint", fpOf(out)); code != 0 {
		t.Fatal(e)
	}
	if strings.Join(kinds, ",") != "normal" {
		t.Fatalf("accept must use the full PushGuard: %v", kinds)
	}
	// and reject, on a fresh pair
	h2 := twoHosts(t)
	runJoinCmd("request", "--config", h2.b.Path(), "--label", "second")
	kinds = nil
	if code, _, e := runJoinCmd("reject", "second", "--config", h2.a.Path()); code != 0 {
		t.Fatal(e)
	}
	if strings.Join(kinds, ",") != "normal" {
		t.Fatalf("reject: %v", kinds)
	}
}

func TestJoinCLI_RequestGuidanceIsReviewFirst(t *testing.T) {
	h := twoHosts(t)
	_, _, e := runJoinCmd("request", "--config", h.b.Path(), "--label", "new-host")
	if !strings.Contains(e, "join status") || !strings.Contains(e, "Only if every key is expected") {
		t.Errorf("guidance must send the person to review first:\n%s", e)
	}
	if strings.Contains(e, "then run `claude-memsync recipients accept --yes` here") {
		t.Errorf("old blind-accept wording:\n%s", e)
	}
}
