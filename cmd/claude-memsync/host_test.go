package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/testenv"
)

var hostNow = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

func hdays(n int) string { return hostNow.Add(-time.Duration(n) * 24 * time.Hour).Format(time.RFC3339) }

type fakeKey struct {
	ID       int64
	Title    string
	RO       bool
	Created  string
	LastUsed string // "" -> null
}

// fakeGH is an httptest GitHub API: an org's repos and their deploy keys. It
// records every request, so a test can prove what was and was not called.
type fakeGH struct {
	mu           sync.Mutex
	repos        []string
	keys         map[string][]fakeKey
	forbidDelete bool
	onDelete     func(repo string) // called while a DELETE is being served
	calls        []string
	srv          *httptest.Server
}

func newFakeGH(t *testing.T) *fakeGH {
	t.Helper()
	f := &fakeGH{keys: map[string][]fakeKey{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGH) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	p := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case r.Method == "GET" && len(p) == 3 && p[0] == "orgs" && p[2] == "repos":
		var out []map[string]any
		for _, n := range f.repos {
			out = append(out, map[string]any{"name": n, "private": true, "visibility": "private"})
		}
		_ = json.NewEncoder(w).Encode(out)
	case r.Method == "GET" && len(p) == 3 && p[0] == "repos":
		_ = json.NewEncoder(w).Encode(map[string]any{"name": p[2], "private": true, "visibility": "private"})
	case r.Method == "GET" && len(p) == 4 && p[0] == "repos" && p[3] == "keys":
		ks := f.keys[p[2]]
		page := 1
		fmt.Sscan(r.URL.Query().Get("page"), &page)
		// two keys per page, to exercise Link pagination
		lo, hi := (page-1)*2, page*2
		if lo > len(ks) {
			lo = len(ks)
		}
		if hi > len(ks) {
			hi = len(ks)
		}
		if hi < len(ks) {
			w.Header().Set("Link", fmt.Sprintf(`<%s%s?page=%d>; rel="next"`, f.srv.URL, r.URL.Path, page+1))
		}
		var out []map[string]any
		for _, k := range ks[lo:hi] {
			m := map[string]any{"id": k.ID, "title": k.Title, "read_only": k.RO, "created_at": k.Created, "key": "ssh-ed25519 AAAA"}
			if k.LastUsed != "" {
				m["last_used"] = k.LastUsed
			} else {
				m["last_used"] = nil
			}
			out = append(out, m)
		}
		_ = json.NewEncoder(w).Encode(out)
	case r.Method == "DELETE" && len(p) == 5 && p[0] == "repos" && p[3] == "keys":
		if f.forbidDelete {
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"message":"Resource not accessible by personal access token"}`))
			return
		}
		if f.onDelete != nil {
			f.onDelete(p[2])
		}
		var id int64
		fmt.Sscan(p[4], &id)
		ks := f.keys[p[2]]
		for i, k := range ks {
			if k.ID == id {
				f.keys[p[2]] = append(ks[:i:i], ks[i+1:]...)
				w.WriteHeader(204)
				return
			}
		}
		w.WriteHeader(404)
	default:
		w.WriteHeader(404)
	}
}

func (f *fakeGH) deletes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "DELETE ") {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeGH) nonGET() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "GET ") {
			out = append(out, c)
		}
	}
	return out
}

// hostFixture is a fake API plus a config pointing at it with a token.
func hostFixture(t *testing.T) (*fakeGH, config.Config) {
	t.Helper()
	initEnv(t)
	skipAPIHostCheck = true
	t.Cleanup(func() { skipAPIHostCheck = false })
	t.Setenv("MEMSYNC_GITHUB_TOKEN", "")
	t.Setenv("HOME", t.TempDir())
	f := newFakeGH(t)
	cfg := config.Defaults()
	cfg.SyncDir = t.TempDir()
	cfg.GitHubAPIURL = f.srv.URL
	cfg.GitHubHost = "github.com"
	cfg.GitHubOrg = "Org"
	cfg.RemoteURL = "git@github.com:Org/mem-personal.git"
	tok := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(tok, []byte(cmdTok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.GitHubTokenFile = tok
	return f, cfg
}

// localRecipients makes a work-tree dir holding an .age-recipients file with
// one line per label ("" -> unlabelled).
func localRecipients(t *testing.T, labels ...string) string {
	t.Helper()
	dir := t.TempDir()
	var keys []string
	for i, l := range labels {
		_, rc := newTestIdentity(t, fmt.Sprintf("id%d", i))
		if l != "" {
			rc += " # host:" + l
		}
		keys = append(keys, rc)
	}
	if err := agecrypt.WriteRecipients(dir, keys); err != nil {
		t.Fatal(err)
	}
	return dir
}

func newTestIdentity(t *testing.T, name string) (string, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	rc, err := agecrypt.GenerateIdentity(p)
	if err != nil {
		t.Fatal(err)
	}
	return p, rc
}

func hostDo(t *testing.T, hc *hostCtx, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	hc.now = hostNow
	code := hostRun(hc, &out, &errb, args)
	return code, out.String(), errb.String()
}

func TestHostList_GroupsAndFlags(t *testing.T) {
	f, cfg := hostFixture(t)
	f.repos = []string{"mem-personal", "mem-project-x", "website", "mem-bogus"}
	f.keys["mem-personal"] = []fakeKey{
		{1, "vm-a", false, hdays(90), hdays(2)},
		{2, "vm-old", false, hdays(200), hdays(60)},
	}
	f.keys["mem-project-x"] = []fakeKey{ // three keys: the API paginates at two
		{3, "vm-a", true, hdays(90), hdays(5)},
		{4, "vm-old", true, hdays(200), hdays(70)},
		{5, "vm-new", false, hdays(1), ""},
	}
	f.keys["website"] = []fakeKey{{9, "vm-a", false, hdays(1), hdays(1)}}
	dirP := localRecipients(t, "", "vm-a", "vm-old")
	dirX := localRecipients(t, "vm-a")
	hc := &hostCtx{cfg: cfg, org: "Org", local: []localRepo{{Name: "mem-personal", Dir: dirP}, {Name: "mem-project-x", Dir: dirX}}}
	code, out, errb := hostDo(t, hc, "list", "--json")
	if code != 0 {
		t.Fatalf("code %d: %s", code, errb)
	}
	var rep struct {
		Hosts []struct {
			Label    string `json:"label"`
			Stale    bool   `json:"stale"`
			Expired  bool   `json:"expired"`
			LastUsed string `json:"lastUsed"`
			Keys     []struct {
				Repo     string
				ReadOnly bool `json:"readOnly"`
			} `json:"keys"`
			Recipient map[string]bool `json:"recipient"`
		} `json:"hosts"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	byL := map[string]int{}
	for i, h := range rep.Hosts {
		byL[h.Label] = i
	}
	if len(rep.Hosts) != 3 {
		t.Fatalf("want vm-a, vm-old, vm-new; got %s", out)
	}
	a, old, nw := rep.Hosts[byL["vm-a"]], rep.Hosts[byL["vm-old"]], rep.Hosts[byL["vm-new"]]
	if len(a.Keys) != 2 || a.Stale || !a.Recipient["mem-personal"] || !a.Recipient["mem-project-x"] {
		t.Errorf("vm-a: %+v", a)
	}
	if !old.Stale || !old.Recipient["mem-personal"] || old.Recipient["mem-project-x"] {
		t.Errorf("vm-old: %+v", old)
	}
	if nw.Stale || len(nw.Keys) != 1 {
		t.Errorf("vm-new: %+v", nw)
	}
	if strings.Contains(out, "website") {
		t.Error("a repo outside mem-* must be ignored")
	}
	for _, c := range f.calls {
		if strings.Contains(c, "/website/") || strings.Contains(c, "mem-bogus") {
			t.Errorf("non-mem repo was queried: %s", c)
		}
	}
	if n := len(f.nonGET()); n != 0 {
		t.Errorf("list must be read-only, made %v", f.nonGET())
	}
	if strings.Contains(out, "AAAA") || strings.Contains(out, cmdTok) {
		t.Error("no key material or token in the output")
	}
}

func TestHostList_TextTableAndStaleDays(t *testing.T) {
	f, cfg := hostFixture(t)
	f.repos = []string{"mem-personal"}
	f.keys["mem-personal"] = []fakeKey{{1, "vm-a", false, hdays(90), hdays(10)}}
	hc := &hostCtx{cfg: cfg, org: "Org", local: []localRepo{{Name: "mem-personal", Dir: localRecipients(t, "vm-a")}}}
	code, out, errb := hostDo(t, hc, "list")
	if code != 0 || !strings.Contains(out, "vm-a") || strings.Contains(out, "stale") {
		t.Fatalf("code %d\n%s%s", code, out, errb)
	}
	if !strings.Contains(out, "mem-personal") || !strings.Contains(out, "rw") {
		t.Errorf("table should show the repo and rw: %s", out)
	}
	code, out, _ = hostDo(t, hc, "list", "--stale-days", "5")
	if code != 0 || !strings.Contains(out, "stale") {
		t.Errorf("--stale-days 5 should flag a key used 10 days ago:\n%s", out)
	}
	if code, _, errb := hostDo(t, hc, "list", "--stale-days", "0"); code != 2 || !strings.Contains(errb, "stale-days") {
		t.Errorf("a bad --stale-days must be a usage error: %d %s", code, errb)
	}
}

func TestHostList_RegistryExpiryAndCheckKeys(t *testing.T) {
	f, cfg := hostFixture(t)
	f.repos = []string{"mem-personal"}
	f.keys["mem-personal"] = []fakeKey{{1, "vm-a", false, hdays(90), hdays(1)}, {2, "vm-b", false, hdays(90), hdays(1)}}
	reg := t.TempDir()
	w := func(rel, body string) {
		p := filepath.Join(reg, filepath.FromSlash(rel))
		_ = os.MkdirAll(filepath.Dir(p), 0o700)
		_ = os.WriteFile(p, []byte(body), 0o600)
	}
	w("hosts/vm-a/expiry", "2026-05-01\n")
	fp := "SHA256:" + strings.Repeat("Q", 43)
	w("hosts/vm-a/check-key", `{"fingerprint":"`+fp+`","app":"chk"}`)
	w("hosts/vm-b/routes.json", "{}")
	hc := &hostCtx{cfg: cfg, org: "Org", registryDir: reg}
	code, out, errb := hostDo(t, hc, "list")
	if code != 0 {
		t.Fatalf("%d %s", code, errb)
	}
	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "vm-a") {
			line = l
		}
	}
	if !strings.Contains(line, "expired") || !strings.Contains(line, fp) {
		t.Errorf("vm-a line should show expired and the check-key fingerprint: %q", line)
	}
	if !strings.Contains(out, "check keys: chk 1 live") || !strings.Contains(out, "as recorded in the registry") {
		t.Errorf("live check key count (with its caveat) missing:\n%s", out)
	}
	// warn threshold
	code, out, errb = hostDo(t, hc, "list", "--check-key-warn", "1")
	if code != 0 || !strings.Contains(errb+out, "at most 25") {
		t.Errorf("a threshold of 1 should warn:\n%s%s", out, errb)
	}
}

func TestHostList_NeedsTokenAndExplainsMissingPermission(t *testing.T) {
	f, cfg := hostFixture(t)
	f.repos = []string{"mem-personal"}
	f.keys["mem-personal"] = nil
	cfg2 := cfg
	cfg2.GitHubTokenFile = ""
	t.Setenv("MEMSYNC_GITHUB_TOKEN", "")
	if code, _, errb := hostDo(t, &hostCtx{cfg: cfg2, org: "Org"}, "list"); code != 1 || !strings.Contains(errb, "token") {
		t.Errorf("no token: %d %s", code, errb)
	}
	// deploy keys listed with 403: say what the token needs
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/repos") {
			_ = json.NewEncoder(w).Encode([]map[string]any{{"name": "mem-personal"}})
			return
		}
		w.WriteHeader(403)
	})
	code, _, errb := hostDo(t, &hostCtx{cfg: cfg, org: "Org"}, "list")
	if code != 1 || !strings.Contains(errb, "Administration") {
		t.Errorf("403 on keys should name the permission: %d %s", code, errb)
	}
}

func TestHostCheckKey_RecordsOwnFingerprint(t *testing.T) {
	_, cfg := hostFixture(t)
	reg := t.TempDir()
	if out, err := execGit(reg, "init", "-q", "-b", "main"); err != nil {
		t.Fatal(out, err)
	}
	hc := &hostCtx{cfg: cfg, registryDir: reg, ownLabels: []string{"vm-a"}}
	fp := "SHA256:" + strings.Repeat("Z", 43)
	code, _, errb := hostDo(t, hc, "check-key", "--fingerprint", fp, "--app", "chk")
	if code != 0 {
		t.Fatalf("%d %s", code, errb)
	}
	b, err := os.ReadFile(filepath.Join(reg, "hosts", "vm-a", "check-key"))
	if err != nil || !strings.Contains(string(b), fp) {
		t.Errorf("%v %s", err, b)
	}
	if code, _, errb := hostDo(t, hc, "check-key", "--fingerprint", "-----BEGIN PRIVATE KEY-----", "--app", "chk"); code == 0 {
		t.Errorf("key text must be refused: %s", errb)
	}
	if code, _, _ := hostDo(t, &hostCtx{cfg: cfg, registryDir: reg}, "check-key", "--fingerprint", fp, "--app", "chk"); code == 0 {
		t.Error("a host without a hostLabel cannot record a key")
	}
	if code, _, errb := hostDo(t, hc, "check-key", "--clear"); code != 0 {
		t.Fatalf("%d %s", code, errb)
	}
	if _, err := os.Stat(filepath.Join(reg, "hosts", "vm-a", "check-key")); err == nil {
		t.Error("--clear should remove the record")
	}
}

func TestHostList_ViaConfigFlag(t *testing.T) {
	f, cfg := hostFixture(t)
	f.repos = []string{"mem-personal"}
	f.keys["mem-personal"] = []fakeKey{{1, "vm-a", false, hdays(9), hdays(1)}}
	cfg.HostLabel = "vm-a"
	if err := os.MkdirAll(cfg.SyncDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := hostCmd(&out, &errb, []string{"list", "--config", cfg.Path()}); code != 0 || !strings.Contains(out.String(), "vm-a") {
		t.Errorf("%s%s", out.String(), errb.String())
	}
}

func execGit(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	return string(out), err
}

func rdays(n int) string {
	return time.Now().Add(-time.Duration(n) * 24 * time.Hour).Format(time.RFC3339)
}

func TestDoctor_HostSummaryAndCheckKeyWarning(t *testing.T) {
	f, cfg := hostFixture(t)
	old := hostResolver
	hostResolver = func(a string) (string, error) { return a, nil }
	t.Cleanup(func() { hostResolver = old })
	f.repos = []string{"mem-personal"}
	f.keys["mem-personal"] = []fakeKey{
		{1, "vm-a", false, rdays(900), rdays(2)},
		{2, "vm-stale", false, rdays(900), rdays(400)},
	}
	if err := os.MkdirAll(cfg.SyncDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := doctorCmd(&out, &errb, []string{"--config", cfg.Path()}); code != 0 {
		t.Fatalf("%d %s%s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "hosts: 1 stale (vm-stale), 0 expired") {
		t.Errorf("doctor should summarise stale hosts:\n%s", out.String())
	}
	// nothing stale: silent
	f.keys["mem-personal"] = f.keys["mem-personal"][:1]
	out.Reset()
	errb.Reset()
	if code := doctorCmd(&out, &errb, []string{"--config", cfg.Path()}); code != 0 || strings.Contains(out.String(), "hosts:") {
		t.Errorf("no stale hosts, no summary: %d %s", code, out.String())
	}
}

func TestInit_HostLabelLabelsOwnRecipientAndRecipientsAddTakesHost(t *testing.T) {
	cfg, _ := initFixture(t)
	t.Setenv("HOME", t.TempDir())
	ms, mm := testenv.Binaries(t)
	cfg.MergeDriverPath = mm
	cfg.HostLabel = "vm-a"
	enc := &encryptSpec{IdentityPath: filepath.Join(t.TempDir(), "k", "identity.txt"), MemsyncExe: ms}
	if err := bootstrapWith(cfg, false, nil, enc); err != nil {
		t.Fatal(err)
	}
	labels, err := agecrypt.ListLabels(cfg.SyncDir)
	if err != nil || len(labels) != 1 || labels[0] != "vm-a" {
		t.Fatalf("own recipient should carry the label: %v %v", labels, err)
	}
	if err := agecrypt.CheckKeyed(gitwt.New(cfg.SyncDir)); err != nil {
		t.Errorf("a labelled recipient must still decrypt: %v", err)
	}
	_, rc := newTestIdentity(t, "other")
	var out, errb bytes.Buffer
	if code := recipientsCmd(&out, &errb, []string{"add", rc, "--host", "vm-b", "--config", cfg.Path()}); code != 0 {
		t.Fatalf("%d %s", code, errb.String())
	}
	labels, _ = agecrypt.ListLabels(cfg.SyncDir)
	if strings.Join(labels, ",") != "vm-a,vm-b" {
		t.Errorf("labels = %v", labels)
	}
	_, rc2 := newTestIdentity(t, "other2")
	if code := recipientsCmd(&out, &errb, []string{"add", rc2, "--host", "Bad Label", "--config", cfg.Path()}); code == 0 {
		t.Error("an invalid label must be refused")
	}
}
