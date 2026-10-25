package ghapp

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAppsRoundTripAndLookup(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "apps.json")
	if err := SaveApp(p, App{Name: "a", AppID: "1", Org: "o", KeyFile: "/k"}); err != nil {
		t.Fatal(err)
	}
	if err := SaveApp(p, App{Name: "b", AppID: "2", Org: "o", KeyCmd: Argv{"secret-tool", "read", "x"}}); err != nil {
		t.Fatal(err)
	}
	if err := SaveApp(p, App{Name: "a", AppID: "3", Org: "o2", KeyFile: "/k2"}); err != nil { // replace
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(p)
		if st.Mode().Perm() != 0o600 {
			t.Errorf("apps.json mode = %o", st.Mode().Perm())
		}
	}
	a, err := LookupApp(p, "a")
	if err != nil || a.AppID != "3" || a.Org != "o2" {
		t.Fatalf("a = %+v %v", a, err)
	}
	b, _ := LookupApp(p, "b")
	if len(b.KeyCmd) != 3 || b.KeyCmd[0] != "secret-tool" || b.KeyFile != "" {
		t.Errorf("b = %+v", b)
	}
	if _, err := LookupApp(p, "nope"); err == nil {
		t.Error("unknown app accepted")
	}
}

func TestAppsFileMustBe0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits")
	}
	p := filepath.Join(t.TempDir(), "apps.json")
	os.WriteFile(p, []byte(`[{"name":"a","appId":"1","org":"o","keyFile":"/k"}]`), 0o644)
	if _, err := LookupApp(p, "a"); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Errorf("0644 apps.json accepted: %v", err)
	}
}

func TestAppValidate(t *testing.T) {
	bad := []App{
		{AppID: "1", Org: "o", KeyFile: "/k"},                               // no name
		{Name: "a", Org: "o", KeyFile: "/k"},                                // no id
		{Name: "a", AppID: "1", Org: "o"},                                   // no key source
		{Name: "a", AppID: "1", Org: "o", KeyFile: "/k", KeyCmd: Argv{"x"}}, // both
		{Name: "a", AppID: "1", Org: "bad/org", KeyFile: "/k"},              // bad org
		{Name: "a", AppID: "x1", Org: "o", KeyFile: "/k"},                   // non-numeric id
	}
	for i, a := range bad {
		if err := a.Validate(); err == nil {
			t.Errorf("case %d accepted: %+v", i, a)
		}
	}
	if err := (App{Name: "a", AppID: "1", Org: "o", KeyFile: "/k"}).Validate(); err != nil {
		t.Error(err)
	}
}

func TestAppMinter(t *testing.T) {
	k := genKey(t)
	gh := newFakeGH(t, &k.PublicKey)
	kp := filepath.Join(t.TempDir(), "k.pem")
	os.WriteFile(kp, pkcs1PEM(k), 0o600)
	m, err := (App{Name: "a", AppID: "9", Org: testOrg, KeyFile: kp, APIBase: gh.URL}).Minter()
	if err != nil {
		t.Fatal(err)
	}
	tok, err := m.Token(t.Context())
	if err != nil || !strings.HasPrefix(tok, testToken) {
		t.Fatalf("tok=%q err=%v", tok, err)
	}
	tok2, _ := m.Token(t.Context())
	if tok2 != tok || gh.mints != 1 {
		t.Error("minter did not cache")
	}
}

func TestKeyCmdJSONArrayAndStringForms(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "apps.json")
	raw := `[
 {"name":"arr","appId":"1","org":"o","keyCmd":["secret-tool","read","vault://My Vault/Item Name/field"]},
 {"name":"str","appId":"2","org":"o","keyCmd":"secret-tool read \"vault://My Vault/Item Name/field\""}
]`
	os.WriteFile(p, []byte(raw), 0o600)
	for _, n := range []string{"arr", "str"} {
		a, err := LookupApp(p, n)
		if err != nil || len(a.KeyCmd) != 3 || a.KeyCmd[2] != "vault://My Vault/Item Name/field" {
			t.Errorf("%s: %+v %v", n, a, err)
		}
	}
	// saved back as an array, so spaces survive a round trip
	if err := SaveApp(p, App{Name: "n", AppID: "3", Org: "o", KeyCmd: Argv{"secret-tool", "read", "a b"}}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), `"a b"`) || !strings.Contains(string(b), `"keyCmd": [`) {
		t.Errorf("not saved as an array: %s", b)
	}
	os.WriteFile(p, []byte(`[{"name":"x","appId":"1","org":"o","keyCmd":5}]`), 0o600)
	if _, err := LookupApp(p, "x"); err == nil {
		t.Error("numeric keyCmd accepted")
	}
}

func TestAPIBaseDefaultsAndValidation(t *testing.T) {
	a := App{Name: "a", AppID: "1", Org: "o", KeyFile: "/k"}
	if a.API() != "https://api.github.com" {
		t.Errorf("default = %q", a.API())
	}
	a.APIBase = "https://ghe.example.test/api/v3/"
	if a.API() != "https://ghe.example.test/api/v3" || a.Validate() != nil {
		t.Errorf("ghe: %q %v", a.API(), a.Validate())
	}
	for _, bad := range []string{"http://ghe.example.test", "ftp://x", "https://", "https://u:p@x.test", "api.github.com", "https://x.test?a=b"} {
		a.APIBase = bad
		if a.Validate() == nil {
			t.Errorf("apiBase %q accepted", bad)
		}
	}
	a.APIBase = "http://127.0.0.1:9999" // loopback http is for tests
	if a.Validate() != nil {
		t.Error("loopback http refused")
	}
}

func TestMinterUsesOnlyTheEntrysAPIBase(t *testing.T) {
	k := genKey(t)
	good := newFakeGH(t, &k.PublicKey)
	other := newFakeGH(t, &k.PublicKey)
	kp := filepath.Join(t.TempDir(), "k.pem")
	os.WriteFile(kp, pkcs1PEM(k), 0o600)
	m, _ := (App{Name: "a", AppID: "9", Org: testOrg, KeyFile: kp, APIBase: good.URL}).Minter()
	if _, err := m.Token(t.Context()); err != nil {
		t.Fatal(err)
	}
	if good.lookups != 1 || other.lookups != 0 {
		t.Errorf("good=%d other=%d", good.lookups, other.lookups)
	}
}

func TestAppsFileRefusesHardLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no link counts")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "apps.json")
	os.WriteFile(p, []byte(`[{"name":"a","appId":"1","org":"o","keyFile":"/k"}]`), 0o600)
	if err := os.Link(p, filepath.Join(dir, "other.json")); err != nil {
		t.Skip("no hard links:", err)
	}
	if _, err := LookupApp(p, "a"); err == nil || !strings.Contains(err.Error(), "hard link") {
		t.Errorf("hard-linked apps.json accepted: %v", err)
	}
}
