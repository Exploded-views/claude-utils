package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/config"
)

const cmdTok = "tok-CMD-SECRET-987654"

type repoJSON struct {
	Name       string `json:"name"`
	Private    *bool  `json:"private,omitempty"`
	Visibility string `json:"visibility,omitempty"`
}

func tb(b bool) *bool { return &b }

func guardCfg(t *testing.T, h http.HandlerFunc, withToken bool) config.Config {
	t.Helper()
	initEnv(t)
	t.Setenv("GIT_SSH_COMMAND", "")
	t.Setenv("GIT_SSH", "")
	skipAPIHostCheck = true
	t.Cleanup(func() { skipAPIHostCheck = false })
	t.Setenv("MEMSYNC_GITHUB_TOKEN", "")
	t.Setenv("HOME", t.TempDir())
	old := hostResolver
	hostResolver = func(a string) (string, error) {
		if strings.Contains(a, "evil") {
			return "gitlab.com", nil
		}
		if strings.Contains(a, ".") {
			return a, nil // a real hostname resolves to itself
		}
		return "github.com", nil
	}
	t.Cleanup(func() { hostResolver = old })
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cfg := config.Defaults()
	cfg.SyncDir = t.TempDir()
	cfg.GitHubAPIURL = srv.URL
	cfg.PrivateOnly = true
	cfg.GitHubHost = "github.com"
	cfg.RemoteURL = "git@gh-mem-personal:Org/mem-personal.git"
	gitInitRemote(t, cfg.SyncDir, cfg.RemoteURL)
	if withToken {
		f := filepath.Join(t.TempDir(), "tok")
		if err := os.WriteFile(f, []byte(cmdTok+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		cfg.GitHubTokenFile = f
	}
	return cfg
}

func jsonH(code int, v any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
}

func TestGuardRemoteInit(t *testing.T) {
	pub := guardCfg(t, jsonH(200, repoJSON{Name: "mem-personal", Private: tb(false), Visibility: "public"}), true)
	if err := guardRemote(pub); err == nil {
		t.Error("public must be refused")
	}
	ok := guardCfg(t, jsonH(200, repoJSON{Name: "mem-personal", Private: tb(true), Visibility: "private"}), true)
	if err := guardRemote(ok); err != nil {
		t.Errorf("private should pass: %v", err)
	}
	nt := guardCfg(t, jsonH(200, repoJSON{Name: "mem-personal", Private: tb(true), Visibility: "private"}), false)
	if err := guardRemote(nt); err == nil {
		t.Error("init without a token cannot verify and must refuse")
	}
	bad := guardCfg(t, jsonH(200, repoJSON{}), true)
	bad.RemoteURL = "git@github.com:Org/claude-memories.git"
	if err := guardRemote(bad); err == nil {
		t.Error("non-mem remote must be refused")
	}
	apiErr := guardCfg(t, jsonH(500, map[string]string{"message": "x"}), true)
	if err := guardRemote(apiErr); err == nil {
		t.Error("api error must refuse")
	}
}

func TestDaemonGuard(t *testing.T) {
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, f) }

	nt := guardCfg(t, jsonH(200, repoJSON{}), false)
	if err := daemonGuard(nt, logf); err != nil {
		t.Errorf("no token must not block: %v", err)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "init") {
		t.Errorf("want one log naming init/doctor, got %v", logs)
	}

	logs = nil
	pub := guardCfg(t, jsonH(200, repoJSON{Name: "mem-personal", Private: tb(false), Visibility: "public"}), true)
	if err := daemonGuard(pub, logf); err == nil {
		t.Error("public with token must block start")
	}
	ok := guardCfg(t, jsonH(200, repoJSON{Name: "mem-personal", Private: tb(true), Visibility: "private"}), true)
	if err := daemonGuard(ok, logf); err != nil {
		t.Errorf("private should start: %v", err)
	}
}

func orgList(repos []repoJSON) http.HandlerFunc { return jsonH(200, repos) }

func TestDoctorExitCodes(t *testing.T) {
	var out, errb bytes.Buffer
	cfg := guardCfg(t, orgList([]repoJSON{
		{Name: "mem-project-x", Private: tb(false), Visibility: "public"},
		{Name: "website", Private: tb(false), Visibility: "public"},
	}), true)
	cfg.RemoteURL = ""
	if code := doctorRun(cfg, "Org", &out, &errb); code == 0 {
		t.Error("public mem-* must exit non-zero")
	}
	if !strings.Contains(out.String()+errb.String(), "mem-project-x") {
		t.Errorf("must name the repo: %q %q", out.String(), errb.String())
	}
	if strings.Contains(out.String()+errb.String(), "website") || strings.Contains(out.String()+errb.String(), cmdTok) {
		t.Error("output must not name ignored repos or the token")
	}

	out.Reset()
	errb.Reset()
	clean := guardCfg(t, orgList([]repoJSON{
		{Name: "website", Private: tb(false), Visibility: "public"},
		{Name: "mem-personal", Private: tb(true), Visibility: "private"},
	}), true)
	clean.RemoteURL = ""
	if code := doctorRun(clean, "Org", &out, &errb); code != 0 {
		t.Errorf("only non-mem public must exit 0, got %d: %s %s", code, out.String(), errb.String())
	}

	nt := guardCfg(t, orgList(nil), false)
	if code := doctorRun(nt, "Org", &out, &errb); code == 0 {
		t.Error("doctor without a token must fail")
	}
}

func TestDoctorJoinedNonMemFails(t *testing.T) {
	var out, errb bytes.Buffer
	cfg := guardCfg(t, orgList(nil), true)
	cfg.RemoteURL = "git@github.com:Org/claude-memories.git"
	if code := doctorRun(cfg, "Org", &out, &errb); code == 0 {
		t.Error("joined non-mem repo must fail doctor")
	}
}

func TestRepoCreateVerifies(t *testing.T) {
	var posted map[string]any
	h := func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			_ = json.NewDecoder(r.Body).Decode(&posted)
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(repoJSON{Name: "mem-personal", Private: tb(true), Visibility: "private"})
			return
		}
		_ = json.NewEncoder(w).Encode(repoJSON{Name: "mem-personal", Private: tb(true), Visibility: "private"})
	}
	cfg := guardCfg(t, h, true)
	var out, errb bytes.Buffer
	if code := repoCreateRun(cfg, "Org", "mem-personal", &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if posted["private"] != true || posted["visibility"] != "private" {
		t.Errorf("posted %v", posted)
	}
	if code := repoCreateRun(cfg, "Org", "website", &out, &errb); code == 0 {
		t.Error("non-mem create must be refused")
	}
}

func TestInstallTimerWritesUnits(t *testing.T) {
	dir := t.TempDir()
	var out, errb bytes.Buffer
	if code := installTimerRun("linux", "/x/claude-memsync", "example-org", "/c.json", "/tok", dir, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	b, err := os.ReadFile(filepath.Join(dir, "claude-memsync-doctor.timer"))
	if err != nil || !strings.Contains(string(b), "OnCalendar=daily") {
		t.Errorf("timer file: %v %s", err, b)
	}
}

func TestGuardOffByDefault(t *testing.T) {
	// Legacy name, no token, API that would refuse everything: off means untouched.
	cfg := guardCfg(t, jsonH(500, map[string]string{}), false)
	cfg.PrivateOnly = false
	cfg.RemoteURL = "git@github.com:someone/claude-memories.git"
	if err := guardRemote(cfg); err != nil {
		t.Errorf("init guard must be off: %v", err)
	}
	var logs []string
	if err := daemonGuard(cfg, func(f string, a ...any) { logs = append(logs, f) }); err != nil || len(logs) != 0 {
		t.Errorf("daemon guard must be silent and off: %v %v", err, logs)
	}
	if config.Defaults().PrivateOnly {
		t.Error("PrivateOnly must default to false")
	}
}

func TestRepoCreateOrgResolution(t *testing.T) {
	var path string
	h := func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			path = r.URL.Path
			w.WriteHeader(201)
		}
		_ = json.NewEncoder(w).Encode(repoJSON{Name: "mem-personal", Private: tb(true), Visibility: "private"})
	}
	var out, errb bytes.Buffer
	cfg := guardCfg(t, h, true)
	cfg.GitHubOrg = "CfgOrg"
	if repoCreateRun(cfg, "FlagOrg", "mem-personal", &out, &errb) != 0 || path != "/orgs/FlagOrg/repos" {
		t.Errorf("flag wins: %s", path)
	}
	repoCreateRun(cfg, "", "mem-personal", &out, &errb)
	if path != "/orgs/CfgOrg/repos" {
		t.Errorf("config next: %s", path)
	}
	cfg.GitHubOrg = ""
	repoCreateRun(cfg, "", "mem-personal", &out, &errb)
	if path != "/orgs/Org/repos" {
		t.Errorf("remote last: %s", path)
	}
	cfg.RemoteURL = ""
	path = ""
	if repoCreateRun(cfg, "", "mem-personal", &out, &errb) == 0 || path != "" {
		t.Error("no org must refuse without a request")
	}
}

func gitInitRemote(t *testing.T, dir, remote string) {
	t.Helper()
	for _, a := range [][]string{{"init", "-q", "-b", "main"}, {"remote", "add", "origin", remote}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, a...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", a, err, out)
		}
	}
}

func privateH() http.HandlerFunc {
	return jsonH(200, repoJSON{Name: "mem-personal", Private: tb(true), Visibility: "private"})
}

func TestDaemonGuardNoTokenStillChecksRemote(t *testing.T) {
	logf := func(string, ...any) {}
	for name, remote := range map[string]string{
		"non-mem":   "git@github.com:Org/claude-memories.git",
		"gitlab":    "https://gitlab.com/Org/mem-personal.git",
		"alias":     "git@evil:Org/mem-personal.git",
		"local":     "/tmp/x.git",
		"other-ssh": "git@gitlab.com:Org/mem-personal.git",
	} {
		cfg := guardCfg(t, privateH(), false)
		cfg.RemoteURL = remote
		if err := daemonGuard(cfg, logf); err == nil {
			t.Errorf("%s: must be refused even without a token", name)
		}
	}
}

func TestHostRefusals(t *testing.T) {
	for _, remote := range []string{"https://gitlab.com/Org/mem-personal.git", "git@gitlab.com:Org/mem-personal.git", "git@evil:Org/mem-personal.git"} {
		cfg := guardCfg(t, privateH(), true)
		cfg.RemoteURL = remote
		if err := guardRemote(cfg); err == nil {
			t.Errorf("%s: init must refuse", remote)
		}
	}
}

func TestPushURLMustMatchRemote(t *testing.T) {
	cfg := guardCfg(t, privateH(), true)
	// A global insteadOf silently sends pushes elsewhere.
	g := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(g, []byte("[url \"https://example.invalid/x/\"]\n\tinsteadOf = git@gh-mem-personal:Org/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", g)
	if err := daemonGuard(cfg, func(string, ...any) {}); err == nil || !strings.Contains(err.Error(), "push") {
		t.Errorf("daemon must refuse a rewritten push URL: %v", err)
	}
	if err := checkPushURL(cfg, cfg.SyncDir); err == nil {
		t.Error("init-side check must refuse too")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	if err := checkPushURL(cfg, cfg.SyncDir); err != nil {
		t.Errorf("unrewritten URL must pass: %v", err)
	}
	cfg.RemoteURL = "git@gh-mem-personal:Org/mem-personal"
	if err := checkPushURL(cfg, cfg.SyncDir); err != nil {
		t.Errorf("normalisation (.git suffix) must pass: %v", err)
	}
}

// Wiring: init --private-only against a public repo pushes nothing.
func TestRunInitPrivateOnlyPublicPushesNothing(t *testing.T) {
	root := t.TempDir()
	bare := filepath.Join(root, "origin.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", "-b", "main", bare).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	cfg := guardCfg(t, jsonH(200, repoJSON{Name: "mem-personal", Private: tb(false), Visibility: "public"}), true)
	remote := "git@gh-mem-personal:Org/mem-personal.git"
	g := filepath.Join(root, "gitconfig")
	if err := os.WriteFile(g, []byte("[url \""+bare+"/\"]\n\tinsteadOf = git@gh-mem-personal:Org/mem-personal.git/\n[url \""+bare+"\"]\n\tinsteadOf = "+remote+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", g)
	claude := filepath.Join(root, "claude")
	_ = os.MkdirAll(filepath.Join(claude, "p1", "memory"), 0700)
	_ = os.WriteFile(filepath.Join(claude, "p1", "memory", "a.md"), []byte("x\n"), 0600)
	// runInit needs a merge driver sibling; fake one next to the test binary.
	exe, _ := os.Executable()
	drv := filepath.Join(filepath.Dir(exe), "claude-memmerge")
	if _, err := os.Stat(drv); err != nil {
		if werr := os.WriteFile(drv, []byte("#!/bin/sh\n"), 0755); werr != nil {
			t.Skip("cannot place merge driver")
		}
		t.Cleanup(func() { os.Remove(drv) })
	}
	ef, err := os.CreateTemp(root, "stderr")
	if err != nil {
		t.Fatal(err)
	}
	oldErr := os.Stderr
	os.Stderr = ef
	code := runInit([]string{"--remote", remote, "--private-only", "--token-file", cfg.GitHubTokenFile,
		"--github-api-url", cfg.GitHubAPIURL, "--github-host", "github.com",
		"--sync-dir", filepath.Join(root, "sync"), "--claude-dir", claude})
	os.Stderr = oldErr
	eb, _ := os.ReadFile(ef.Name())
	var errb bytes.Buffer
	errb.Write(eb)
	if code == 0 {
		t.Fatalf("init must refuse a public repo: %s", errb.String())
	}
	if !strings.Contains(errb.String(), "not private") {
		t.Errorf("refusal must come from the visibility check: %s", errb.String())
	}
	if out, _ := exec.Command("git", "-C", bare, "rev-list", "--all", "--count").Output(); strings.TrimSpace(string(out)) != "0" {
		t.Errorf("nothing may be pushed, origin has %s commits", out)
	}
}

// Wiring: program.Start with a public repo refuses.
func TestProgramStartRefusesPublicRepo(t *testing.T) {
	cfg := guardCfg(t, jsonH(200, repoJSON{Name: "mem-personal", Private: tb(false), Visibility: "public"}), true)
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	p := &program{configPath: cfg.Path()}
	err := p.Start(nil)
	if err == nil {
		_ = p.Stop(nil)
		t.Fatal("Start must refuse a public repo")
	}
	if !strings.Contains(err.Error(), "not private") {
		t.Errorf("wrong refusal: %v", err)
	}
}

func TestTimerNeedsTokenFile(t *testing.T) {
	var out, errb bytes.Buffer
	if code := installTimerRun("linux", "/x/m", "example-org", "/c.json", "", t.TempDir(), &out, &errb); code == 0 || !strings.Contains(errb.String(), "githubTokenFile") {
		t.Errorf("must refuse without a token file: %d %s", code, errb.String())
	}
	if code := installTimerRun("linux", "/x/m", "bad org", "/c.json", "/tok", t.TempDir(), &out, &errb); code == 0 {
		t.Error("bad org must be refused")
	}
}

var _ = url.PathEscape

func TestGitHostMustMatchAPI(t *testing.T) {
	cfg := guardCfg(t, privateH(), true)
	skipAPIHostCheck = false
	cfg.GitHubHost = "gitlab.com"
	cfg.GitHubAPIURL = ""
	cfg.RemoteURL = "https://gitlab.com/Org/mem-personal.git"
	if err := guardRemote(cfg); err == nil {
		t.Error("githubHost gitlab.com with the default API must be refused")
	}
	if err := daemonGuard(cfg, func(string, ...any) {}); err == nil {
		t.Error("daemon must refuse it too")
	}
}

func TestSSHOverridesRefused(t *testing.T) {
	cfg := guardCfg(t, privateH(), true)
	t.Setenv("GIT_SSH_COMMAND", "ssh -F /elsewhere")
	if err := guardRemote(cfg); err == nil || !strings.Contains(err.Error(), "GIT_SSH_COMMAND") {
		t.Errorf("env override: %v", err)
	}
	t.Setenv("GIT_SSH_COMMAND", "")
	g := filepath.Join(t.TempDir(), "gc")
	if err := os.WriteFile(g, []byte("[core]\n\tsshCommand = ssh -F /elsewhere\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", g)
	if err := daemonGuard(cfg, func(string, ...any) {}); err == nil || !strings.Contains(err.Error(), "core.sshCommand") {
		t.Errorf("config override: %v", err)
	}
	cfg.RemoteURL = "https://github.com/Org/mem-personal.git"
	gitInitRemote2(t, cfg)
	if err := validateRemoteOffline(cfg); err != nil {
		t.Errorf("https remotes are unaffected by ssh overrides: %v", err)
	}
}

func gitInitRemote2(t *testing.T, cfg config.Config) {
	t.Helper()
	_ = exec.Command("git", "-C", cfg.SyncDir, "remote", "set-url", "origin", cfg.RemoteURL).Run()
}

func TestPushWatchBlocksOnMidRunInsteadOf(t *testing.T) {
	cfg := guardCfg(t, privateH(), true)
	allowed := pushWatch(cfg, func(string, ...any) {})
	if allowed == nil || !allowed() {
		t.Fatal("should start allowed")
	}
	g := filepath.Join(t.TempDir(), "gc")
	if err := os.WriteFile(g, []byte("[url \"https://example.invalid/x/\"]\n\tinsteadOf = git@gh-mem-personal:Org/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", g)
	if allowed() {
		t.Fatal("an insteadOf rule added mid-run must block the next push")
	}
}

func TestPushWatchWithoutTokenStillRunsLocalChecks(t *testing.T) {
	cfg := guardCfg(t, privateH(), false)
	allowed := pushWatch(cfg, func(string, ...any) {})
	if allowed == nil {
		t.Fatal("private-only without a token must still install the local precheck")
	}
	if !allowed() {
		t.Fatal("clean state must allow")
	}
	g := filepath.Join(t.TempDir(), "gc")
	if err := os.WriteFile(g, []byte("[url \"https://example.invalid/x/\"]\n\tinsteadOf = git@gh-mem-personal:Org/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", g)
	if allowed() {
		t.Fatal("insteadOf added mid-run must block even without a token")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_SSH_COMMAND", "ssh -F /x")
	if allowed() {
		t.Fatal("ssh override must block even without a token")
	}
	off := guardCfg(t, privateH(), false)
	off.PrivateOnly = false
	if pushWatch(off, func(string, ...any) {}) != nil {
		t.Error("off means no hook")
	}
}

func TestProgramStartWiresPushAllowed(t *testing.T) {
	cfg := guardCfg(t, privateH(), true)
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	p := &program{configPath: cfg.Path()}
	if err := p.Start(nil); err != nil {
		t.Fatal(err)
	}
	defer p.Stop(nil)
	if p.loop == nil || p.loop.PushAllowed == nil {
		t.Fatal("Start must give the loop a PushAllowed hook")
	}
	legacy := guardCfg(t, privateH(), false)
	legacy.PrivateOnly = false
	if err := config.Save(legacy); err != nil {
		t.Fatal(err)
	}
	q := &program{configPath: legacy.Path()}
	if err := q.Start(nil); err != nil {
		t.Fatal(err)
	}
	defer q.Stop(nil)
	if q.loop.PushAllowed != nil {
		t.Error("off must leave the loop unchanged")
	}
}

// Wiring: a private repo but an insteadOf rule sending pushes elsewhere; init must push nothing.
func TestRunInitInsteadOfPushesNothing(t *testing.T) {
	root := t.TempDir()
	bare := filepath.Join(root, "origin.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", "-b", "main", bare).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	cfg := guardCfg(t, privateH(), true)
	remote := "git@gh-mem-personal:Org/mem-personal.git"
	g := filepath.Join(root, "gitconfig")
	if err := os.WriteFile(g, []byte("[url \""+bare+"\"]\n\tinsteadOf = "+remote+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", g)
	claude := filepath.Join(root, "claude")
	_ = os.MkdirAll(filepath.Join(claude, "p1", "memory"), 0700)
	_ = os.WriteFile(filepath.Join(claude, "p1", "memory", "a.md"), []byte("x\n"), 0600)
	exe, _ := os.Executable()
	drv := filepath.Join(filepath.Dir(exe), "claude-memmerge")
	if _, err := os.Stat(drv); err != nil {
		if werr := os.WriteFile(drv, []byte("#!/bin/sh\n"), 0755); werr != nil {
			t.Skip("cannot place merge driver")
		}
		t.Cleanup(func() { os.Remove(drv) })
	}
	ef, err := os.CreateTemp(root, "stderr")
	if err != nil {
		t.Fatal(err)
	}
	oldErr := os.Stderr
	os.Stderr = ef
	code := runInit([]string{"--remote", remote, "--private-only", "--token-file", cfg.GitHubTokenFile,
		"--github-api-url", cfg.GitHubAPIURL, "--github-host", "github.com",
		"--sync-dir", filepath.Join(root, "sync"), "--claude-dir", claude})
	os.Stderr = oldErr
	eb, _ := os.ReadFile(ef.Name())
	if code == 0 || !strings.Contains(string(eb), "different URL") {
		t.Fatalf("init must refuse on the push URL (code %d): %s", code, eb)
	}
	if out, _ := exec.Command("git", "-C", bare, "rev-list", "--all", "--count").Output(); strings.TrimSpace(string(out)) != "0" {
		t.Errorf("nothing may be pushed, origin has %s commits", out)
	}
}
