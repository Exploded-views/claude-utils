package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/ghapp"
)

const appTestToken = "ghs_APPTEST_zzz999"

type appFake struct {
	*httptest.Server
	mu    sync.Mutex
	body  map[string]any
	mints int
	orgs  []string
}

func newAppFake(t *testing.T) *appFake {
	f := &appFake{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			f.orgs = append(f.orgs, strings.Split(r.URL.Path, "/")[2])
			fmt.Fprint(w, `{"id":5}`)
		case r.URL.Path == "/app/installations/5/access_tokens":
			f.mints++
			f.body = nil
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &f.body)
			w.WriteHeader(201)
			fmt.Fprintf(w, `{"token":%q,"expires_at":%q}`, appTestToken, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		case strings.HasPrefix(r.URL.Path, "/repos/"):
			if r.Header.Get("Authorization") != "Bearer "+appTestToken {
				w.WriteHeader(401)
				return
			}
			fmt.Fprint(w, `{"name":"mem-personal","private":true,"visibility":"private"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

// appEnv writes a test key (0600) and an apps.json naming it, and points the
// CLI at the latter.
func appEnv(t *testing.T, apiBase string) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	kp := filepath.Join(dir, "k.pem")
	os.WriteFile(kp, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), 0o600)
	ap := filepath.Join(dir, "apps.json")
	if err := ghapp.SaveApp(ap, ghapp.App{Name: "check", AppID: "11", Org: "orga", KeyFile: kp, APIBase: apiBase}); err != nil {
		t.Fatal(err)
	}
	old := appsFileOverride
	appsFileOverride = ap
	t.Cleanup(func() { appsFileOverride = old })
	minterMu.Lock()
	minterCache = map[string]*ghapp.Minter{}
	minterMu.Unlock()
	return kp
}

func TestAppTokenPrintsToken(t *testing.T) {
	gh := newAppFake(t)
	appEnv(t, gh.URL)
	var out, errb bytes.Buffer
	if code := appTokenCmd(&out, &errb, []string{"--app", "check"}); code != 0 {
		t.Fatalf("code %d: %s", code, errb.String())
	}
	if out.String() != appTestToken+"\n" {
		t.Errorf("stdout = %q", out.String())
	}
	if errb.Len() != 0 {
		t.Errorf("stderr = %q", errb.String())
	}
	if gh.orgs[0] != "orga" {
		t.Errorf("org = %v", gh.orgs)
	}
}

func TestAppTokenOrgOverrideAndNarrowing(t *testing.T) {
	gh := newAppFake(t)
	appEnv(t, gh.URL)
	var out, errb bytes.Buffer
	code := appTokenCmd(&out, &errb, []string{"--app", "check", "--org", "orgb",
		"--perm", "metadata:read", "--perm", "administration:write", "--repo", "mem-personal"})
	if code != 0 {
		t.Fatalf("code %d: %s", code, errb.String())
	}
	if gh.orgs[0] != "orgb" {
		t.Errorf("org override ignored: %v", gh.orgs)
	}
	perms, _ := gh.body["permissions"].(map[string]any)
	if perms["metadata"] != "read" || perms["administration"] != "write" || len(gh.body["repositories"].([]any)) != 1 {
		t.Errorf("body = %v", gh.body)
	}
}

func TestAppTokenErrors(t *testing.T) {
	appEnv(t, "")
	var out, errb bytes.Buffer
	if code := appTokenCmd(&out, &errb, nil); code != 2 {
		t.Errorf("missing --app: %d", code)
	}
	if code := appTokenCmd(&out, &errb, []string{"--app", "nope"}); code != 1 || out.Len() != 0 {
		t.Errorf("unknown app: %d %q", code, out.String())
	}
	if code := appTokenCmd(&out, &errb, []string{"--app", "check", "--perm", "bogus"}); code != 2 {
		t.Errorf("bad --perm: %d", code)
	}
}

func TestAppTokenRefusesLooseKeyAndHidesSecrets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits")
	}
	gh := newAppFake(t)
	kp := appEnv(t, gh.URL)
	os.Chmod(kp, 0o644)
	var out, errb bytes.Buffer
	if code := appTokenCmd(&out, &errb, []string{"--app", "check"}); code == 0 {
		t.Fatal("0644 key accepted")
	}
	if gh.mints != 0 {
		t.Error("minted despite a loose key")
	}
	if strings.Contains(errb.String(), "BEGIN") || out.Len() != 0 {
		t.Errorf("leak: %q %q", out.String(), errb.String())
	}
}

func TestGuardClientUsesGitHubApp(t *testing.T) {
	gh := newAppFake(t)
	appEnv(t, gh.URL)
	cfg := guardCfg(t, nil, false)
	cfg.GitHubAPIURL = gh.URL // equal to the app's apiBase: fine
	cfg.GitHubApp = "check"
	cfg.GitHubTokenFile = "" // the app wins over file, cmd and env
	t.Setenv("MEMSYNC_GITHUB_TOKEN", "env-token-must-lose")
	c, err := guardClient(cfg)
	if err != nil || c == nil {
		t.Fatalf("client=%v err=%v", c, err)
	}
	if err := c.CheckPrivate("orga", "mem-personal"); err != nil {
		t.Fatal(err)
	}
	c2, _ := guardClient(cfg)
	c2.CheckPrivate("orga", "mem-personal")
	if gh.mints != 1 {
		t.Errorf("token not cached across clients: %d mints", gh.mints)
	}
	cfg.GitHubApp = "nope"
	if _, err := guardClient(cfg); err == nil {
		t.Error("unknown app accepted")
	}
}

func TestGuardClientWithAppDefaultsToTheAppsAPIBase(t *testing.T) {
	gh := newAppFake(t)
	appEnv(t, gh.URL)
	cfg := guardCfg(t, nil, false)
	cfg.GitHubAPIURL = "" // unset: calls go where apps.json says
	cfg.GitHubApp = "check"
	cfg.GitHubTokenFile = ""
	skipAPIHostCheck = true
	c, err := guardClient(cfg)
	if err != nil || c == nil {
		t.Fatalf("client=%v err=%v", c, err)
	}
	if err := c.CheckPrivate("orga", "mem-personal"); err != nil {
		t.Fatal(err)
	}
}

// A JWT proves control of the app, so config.json must not be able to point it
// anywhere: a githubApiUrl other than the entry's apiBase gets nothing.
func TestConfigAPIURLNeverReceivesTheAppJWT(t *testing.T) {
	legit := newAppFake(t)
	var evilHits atomic.Int32
	var evilAuth []string
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		evilHits.Add(1)
		evilAuth = append(evilAuth, r.Header.Get("Authorization"))
	}))
	defer evil.Close()
	appEnv(t, legit.URL)
	cfg := guardCfg(t, nil, false)
	cfg.GitHubApp = "check"
	cfg.GitHubTokenFile = ""
	cfg.GitHubAPIURL = evil.URL
	c, err := guardClient(cfg)
	if err == nil || c != nil {
		t.Fatalf("a different githubApiUrl was accepted: %v %v", c, err)
	}
	if evilHits.Load() != 0 {
		t.Errorf("the config's API URL received %d requests: %v", evilHits.Load(), evilAuth)
	}
	if legit.mints != 0 {
		t.Error("minted although the endpoints disagree")
	}
	// and the mint itself ignores the config entirely
	cfg.GitHubAPIURL = ""
	if _, err := guardClient(cfg); err != nil || evilHits.Load() != 0 {
		t.Errorf("err=%v evil=%d", err, evilHits.Load())
	}
	if len(legit.orgs) == 0 {
		t.Error("the JWT never reached the apps.json apiBase")
	}
}

func TestPlantedGitHubAppInUntrustedConfigIsIgnored(t *testing.T) {
	gh := newAppFake(t)
	appEnv(t, gh.URL)
	warnings(t)
	cfg := guardCfg(t, nil, false)
	cfg.GitHubApp = "check"
	cfg.GitHubTokenFile = ""
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	os.Chmod(cfg.Path(), 0o666)
	loaded, err := loadConfig(cfg.Path())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.GitHubApp != "" {
		t.Fatal("githubApp from an untrusted config.json was kept")
	}
	if c, _ := guardClient(loaded); c != nil {
		t.Error("client built from a planted githubApp")
	}
	if gh.mints != 0 || len(gh.orgs) != 0 {
		t.Error("an app token was minted for a planted config")
	}
}

func TestTimerAcceptsApp(t *testing.T) {
	gh := newAppFake(t)
	appEnv(t, gh.URL)
	cfg := guardCfg(t, nil, false)
	cfg.GitHubApp = "check"
	cfg.GitHubTokenFile = ""
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := doctorCmd(&out, &errb, []string{"--config", cfg.Path(), "--org", "orga", "--install-timer", "--unit-dir", t.TempDir()})
	if code != 0 {
		t.Errorf("timer with githubApp refused: %d %s", code, errb.String())
	}
}

func TestDoctorGitHubAppFlagIsHonouredFromAnyConfig(t *testing.T) {
	gh := newAppFake(t)
	appEnv(t, gh.URL)
	cfg := guardCfg(t, nil, false)
	cfg.GitHubTokenFile = ""
	cfg.RemoteURL = ""
	cfg.GitHubAPIURL = gh.URL
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	os.Chmod(cfg.Path(), 0o666) // untrusted config, but the flag is the user's own
	var out, errb bytes.Buffer
	doctorCmd(&out, &errb, []string{"--config", cfg.Path(), "--org", "orga", "--github-app", "check"})
	if gh.mints != 0 && len(gh.orgs) == 0 {
		t.Error("inconsistent fake state")
	}
	if len(gh.orgs) == 0 {
		t.Errorf("--github-app was not used: %s %s", out.String(), errb.String())
	}
}
