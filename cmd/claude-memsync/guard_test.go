package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
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
	t.Setenv("MEMSYNC_GITHUB_TOKEN", "")
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cfg := config.Defaults()
	cfg.SyncDir = t.TempDir()
	cfg.GitHubAPIURL = srv.URL
	cfg.PrivateOnly = true
	cfg.RemoteURL = "git@gh-mem-personal:Org/mem-personal.git"
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
	if code := installTimerRun("linux", "/x/claude-memsync", "example-org", dir, &out, &errb); code != 0 {
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
