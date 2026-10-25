package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/ghapp"
)

var cliPEM = func() string {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
}()

// keyBody is a line of key material that must never be printed.
var keyBody = strings.Split(cliPEM, "\n")[1]

func convServer(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/app-manifests/c0de/conversions" {
			w.WriteHeader(201)
			fmt.Fprintf(w, `{"id":99,"slug":"host-check","pem":%q}`, cliPEM)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func withCallback(t *testing.T, code string) {
	old := registerOnReady
	registerOnReady = func(r ghapp.Ready) {
		go http.Get(r.CallbackURL + "?code=" + code + "&state=" + r.State)
	}
	t.Cleanup(func() { registerOnReady = old })
}

func regArgs(api string, extra ...string) []string {
	return append([]string{"register", "--name", "host-check", "--org", "orga", "--url", "https://example.test/home",
		"--perm", "administration:write", "--perm", "metadata:read",
		"--github-url", "https://github.test", "--api-url", api}, extra...)
}

func TestAppRegisterKeyOut(t *testing.T) {
	apps := filepath.Join(t.TempDir(), "apps.json")
	old := appsFileOverride
	appsFileOverride = apps
	t.Cleanup(func() { appsFileOverride = old })
	api := convServer(t)
	withCallback(t, "c0de")
	keyOut := filepath.Join(t.TempDir(), "host.pem")
	var out, errb bytes.Buffer
	if code := appCmd(&out, &errb, regArgs(api.URL, "--key-out", keyOut)); code != 0 {
		t.Fatalf("code %d: %s", code, errb.String())
	}
	b, err := os.ReadFile(keyOut)
	if err != nil || string(b) != cliPEM {
		t.Fatalf("key file: %q %v", b, err)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(keyOut); st.Mode().Perm() != 0o600 {
			t.Errorf("key mode %o", st.Mode().Perm())
		}
	}
	all := out.String() + errb.String()
	if strings.Contains(all, keyBody) || strings.Contains(all, "BEGIN") {
		t.Errorf("pem printed: %s", all)
	}
	for _, want := range []string{"register-app.html", "file://", "99", "host-check", "https://github.test/apps/host-check/installations/new"} {
		if !strings.Contains(all, want) {
			t.Errorf("output lacks %q:\n%s", want, all)
		}
	}
	a, err := ghapp.LookupApp(apps, "host-check")
	if err != nil || a.AppID != "99" || a.Org != "orga" || a.KeyFile != keyOut {
		t.Errorf("apps.json entry = %+v %v", a, err)
	}
	if st, _ := os.Stat(apps); runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Errorf("apps.json mode %o", st.Mode().Perm())
	}
}

func TestAppRegisterRefusesExistingKeyOut(t *testing.T) {
	api := convServer(t)
	withCallback(t, "c0de")
	keyOut := filepath.Join(t.TempDir(), "host.pem")
	os.WriteFile(keyOut, []byte("precious"), 0o600)
	called := false
	prev := registerOnReady
	registerOnReady = func(r ghapp.Ready) { called = true; go http.Get(r.CallbackURL + "?state=x") } // bail out fast if it ever starts
	t.Cleanup(func() { registerOnReady = prev })
	var out, errb bytes.Buffer
	if code := appCmd(&out, &errb, regArgs(api.URL, "--key-out", keyOut)); code == 0 {
		t.Fatal("existing --key-out accepted")
	}
	if b, _ := os.ReadFile(keyOut); string(b) != "precious" {
		t.Error("existing key file overwritten")
	}
	if called || strings.Contains(out.String(), "register-app.html") {
		t.Error("registration started although --key-out exists (the app would be created and its key lost)")
	}
}

func TestAppRegisterNeedsAKeyDestination(t *testing.T) {
	api := convServer(t)
	var out, errb bytes.Buffer
	if code := appCmd(&out, &errb, regArgs(api.URL)); code != 2 {
		t.Errorf("no --key-out/--key-cmd-store: %d", code)
	}
	if code := appCmd(&out, &errb, regArgs(api.URL, "--key-cmd-store", "tee /dev/null")); code != 2 {
		t.Errorf("--key-cmd-store without --key-cmd: %d", code)
	}
	if code := appCmd(&out, &errb, regArgs(api.URL, "--key-out", "/x", "--key-cmd-store", "y", "--key-cmd", "z")); code != 2 {
		t.Errorf("both destinations: %d", code)
	}
	if code := appCmd(&out, &errb, []string{"register", "--name", "x", "--org", "o", "--key-out", "/x"}); code != 2 {
		t.Errorf("missing --url: %d", code)
	}
}

func TestAppRegisterKeyCmdStore(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs tee")
	}
	apps := filepath.Join(t.TempDir(), "apps.json")
	old := appsFileOverride
	appsFileOverride = apps
	t.Cleanup(func() { appsFileOverride = old })
	api := convServer(t)
	withCallback(t, "c0de")
	stored := filepath.Join(t.TempDir(), "stored")
	var out, errb bytes.Buffer
	code := appCmd(&out, &errb, regArgs(api.URL, "--key-cmd-store", "tee "+stored, "--key-cmd", "cat "+stored))
	if code != 0 {
		t.Fatalf("code %d: %s", code, errb.String())
	}
	if b, _ := os.ReadFile(stored); string(b) != cliPEM {
		t.Errorf("store cmd stdin = %q", b)
	}
	if strings.Contains(out.String()+errb.String(), keyBody) {
		t.Error("pem printed")
	}
	a, err := ghapp.LookupApp(apps, "host-check")
	if err != nil || len(a.KeyCmd) != 2 || a.KeyCmd[0] != "cat" || a.KeyFile != "" {
		t.Errorf("entry = %+v %v", a, err)
	}
}

func TestAppRegisterStoreFailureWritesNoEntry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs false")
	}
	apps := filepath.Join(t.TempDir(), "apps.json")
	old := appsFileOverride
	appsFileOverride = apps
	t.Cleanup(func() { appsFileOverride = old })
	api := convServer(t)
	withCallback(t, "c0de")
	var out, errb bytes.Buffer
	if code := appCmd(&out, &errb, regArgs(api.URL, "--key-cmd-store", "false", "--key-cmd", "cat x")); code == 0 {
		t.Fatal("failed store reported success")
	}
	if _, err := os.Stat(apps); err == nil {
		t.Error("apps.json written although the key was not stored")
	}
	if strings.Contains(out.String()+errb.String(), keyBody) {
		t.Error("pem printed")
	}
}

func TestAppRegisterForgedStateIsIgnoredNotObeyed(t *testing.T) {
	api := convServer(t)
	oldR, oldW := registerOnReady, registerWait
	registerOnReady = func(r ghapp.Ready) { go http.Get(r.CallbackURL + "?code=c0de&state=forged") }
	registerWait = 300 * time.Millisecond
	t.Cleanup(func() { registerOnReady, registerWait = oldR, oldW })
	keyOut := filepath.Join(t.TempDir(), "k.pem")
	var out, errb bytes.Buffer
	if code := appCmd(&out, &errb, regArgs(api.URL, "--key-out", keyOut)); code == 0 {
		t.Fatal("forged state accepted")
	}
	if !strings.Contains(errb.String(), "timed out") {
		t.Errorf("want a timeout (the forged request is ignored), got %q", errb.String())
	}
	if _, err := os.Stat(keyOut); err == nil {
		t.Error("key written after a forged callback")
	}
}

func TestAppRegisterKeyCmdsWithQuotedSpaces(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs tee")
	}
	apps := filepath.Join(t.TempDir(), "apps.json")
	old := appsFileOverride
	appsFileOverride = apps
	t.Cleanup(func() { appsFileOverride = old })
	api := convServer(t)
	withCallback(t, "c0de")
	dir := filepath.Join(t.TempDir(), "my vault")
	os.MkdirAll(dir, 0o700)
	stored := filepath.Join(dir, "item name")
	var out, errb bytes.Buffer
	code := appCmd(&out, &errb, regArgs(api.URL, "--key-cmd-store", `tee "`+stored+`"`, "--key-cmd", `cat "`+stored+`"`))
	if code != 0 {
		t.Fatalf("code %d: %s", code, errb.String())
	}
	if b, _ := os.ReadFile(stored); string(b) != cliPEM {
		t.Errorf("store cmd stdin = %q", b)
	}
	a, err := ghapp.LookupApp(apps, "host-check")
	if err != nil || len(a.KeyCmd) != 2 || a.KeyCmd[1] != stored {
		t.Errorf("entry = %+v %v", a, err)
	}
	if a.APIBase != api.URL {
		t.Errorf("apiBase = %q, want %q", a.APIBase, api.URL)
	}
}
