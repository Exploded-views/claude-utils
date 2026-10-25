package ghapp

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBuildManifestExact(t *testing.T) {
	m := BuildManifest("my-app", "https://example.test/home", "http://127.0.0.1:4242/callback",
		map[string]string{"administration": "write", "metadata": "read"})
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	json.Unmarshal(b, &got)
	want := map[string]any{
		"name":                "my-app",
		"url":                 "https://example.test/home",
		"hook_attributes":     map[string]any{"url": "https://example.invalid", "active": false},
		"redirect_url":        "http://127.0.0.1:4242/callback",
		"public":              false,
		"default_permissions": map[string]any{"administration": "write", "metadata": "read"},
		"default_events":      []any{},
	}
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(want)
	if string(gb) != string(wb) {
		t.Errorf("manifest\n got %s\nwant %s", gb, wb)
	}
	if !strings.Contains(string(b), `"default_events":[]`) {
		t.Errorf("default_events must be [] not null: %s", b)
	}
}

func TestParsePerm(t *testing.T) {
	m, err := ParsePerms([]string{"administration:write", "metadata:read"})
	if err != nil || m["administration"] != "write" || m["metadata"] != "read" {
		t.Fatalf("%v %v", m, err)
	}
	for _, bad := range []string{"", "x", "x:", ":read", "x:owner", "a:b:c"} {
		if _, err := ParsePerms([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

type regFake struct {
	*httptest.Server
	mu    sync.Mutex
	paths []string
	auth  []string
}

const fakePEM = "-----BEGIN RSA PRIVATE KEY-----\nFAKEKEYMATERIAL\n-----END RSA PRIVATE KEY-----\n"

func newRegFake(t *testing.T) *regFake {
	f := &regFake{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.paths = append(f.paths, r.Method+" "+r.URL.Path)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		if r.Method == "POST" && r.URL.Path == "/app-manifests/thecode/conversions" {
			w.WriteHeader(201)
			fmt.Fprintf(w, `{"id":4321,"slug":"my-app","html_url":"https://x/y","pem":%q,"webhook_secret":"WHSECRET","client_secret":"CSECRET"}`, fakePEM)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func hit(url string) {
	resp, err := http.Get(url)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
}

func TestRegisterFlow(t *testing.T) {
	api := newRegFake(t)
	var ready Ready
	var gotHTML string
	var fileMode os.FileMode
	p := RegisterParams{
		Name: "my-app", Org: "orga", Homepage: "https://example.test/home",
		Perms:   map[string]string{"administration": "write", "metadata": "read"},
		WebBase: "https://github.test", APIBase: api.URL, Dir: t.TempDir(),
		OnReady: func(r Ready) {
			ready = r
			b, err := os.ReadFile(r.FormPath)
			if err != nil {
				t.Error(err)
			}
			gotHTML = string(b)
			st, _ := os.Stat(r.FormPath)
			fileMode = st.Mode().Perm()
			go hit(r.CallbackURL + "?code=thecode&state=" + r.State)
		},
	}
	conv, err := Register(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if conv.ID != 4321 || conv.Slug != "my-app" || string(conv.PEM) != fakePEM {
		t.Errorf("conv = %+v", conv)
	}
	if !strings.HasPrefix(ready.FileURL, "file://") || !strings.HasSuffix(ready.FileURL, "/"+baseName(ready.FormPath)) {
		t.Errorf("file url = %q for %q", ready.FileURL, ready.FormPath)
	}
	if !strings.HasPrefix(ready.CallbackURL, "http://127.0.0.1:") || !strings.HasSuffix(ready.CallbackURL, "/callback") {
		t.Errorf("callback = %q", ready.CallbackURL)
	}
	if len(ready.State) < 16 {
		t.Errorf("state too short: %q", ready.State)
	}
	if runtime.GOOS != "windows" && fileMode != 0o600 {
		t.Errorf("form file mode %o", fileMode)
	}
	// form
	m := regexp.MustCompile(`<form[^>]*action="([^"]+)"[^>]*method="post"`).FindStringSubmatch(gotHTML)
	if m == nil {
		t.Fatalf("no POST form in %s", gotHTML)
	}
	if !strings.HasPrefix(html.UnescapeString(m[1]), "https://github.test/organizations/orga/settings/apps/new") {
		t.Errorf("form target = %q", m[1])
	}
	mf := regexp.MustCompile(`name="manifest" value="([^"]*)"`).FindStringSubmatch(gotHTML)
	if mf == nil {
		t.Fatalf("no manifest field in %s", gotHTML)
	}
	var man map[string]any
	if err := json.Unmarshal([]byte(html.UnescapeString(mf[1])), &man); err != nil {
		t.Fatalf("manifest field is not JSON: %v", err)
	}
	if man["redirect_url"] != ready.CallbackURL || man["name"] != "my-app" {
		t.Errorf("manifest = %v", man)
	}
	if st := regexp.MustCompile(`name="state" value="([^"]*)"`).FindStringSubmatch(gotHTML); st == nil || st[1] != ready.State {
		t.Errorf("state field missing or different: %v", st)
	}
	if !strings.Contains(gotHTML, "submit()") {
		t.Error("form does not auto-submit")
	}
	// conversion is unauthenticated
	for i, a := range api.auth {
		if a != "" {
			t.Errorf("request %s sent Authorization %q", api.paths[i], a)
		}
	}
	if len(api.paths) != 1 || api.paths[0] != "POST /app-manifests/thecode/conversions" {
		t.Errorf("paths = %v", api.paths)
	}
}

func baseName(p string) string { return p[strings.LastIndexAny(p, `/\`)+1:] }

func statusOf(url string) int {
	resp, err := http.Get(url)
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// A forged callback must not end the registration: the real one that follows
// still completes it, and nothing was exchanged for the forged one.
func TestRegisterIgnoresWrongStateAndKeepsWaiting(t *testing.T) {
	api := newRegFake(t)
	forged := make(chan int, 1)
	p := RegisterParams{Name: "a", Org: "orga", Homepage: "https://example.test", WebBase: "https://github.test",
		APIBase: api.URL, Dir: t.TempDir(),
		OnReady: func(r Ready) {
			go func() {
				forged <- statusOf(r.CallbackURL + "?code=evilcode&state=WRONG")
				hit(r.CallbackURL + "?code=thecode&state=" + r.State)
			}()
		}}
	conv, err := Register(context.Background(), p)
	if err != nil {
		t.Fatalf("a forged callback aborted the registration: %v", err)
	}
	if st := <-forged; st != http.StatusBadRequest {
		t.Errorf("forged callback got HTTP %d", st)
	}
	if conv.ID != 4321 {
		t.Errorf("conv = %+v", conv)
	}
	if len(api.paths) != 1 || api.paths[0] != "POST /app-manifests/thecode/conversions" {
		t.Errorf("exchanged the wrong code: %v", api.paths)
	}
}

func TestRegisterOnlyWrongStateTimesOutWithoutExchanging(t *testing.T) {
	api := newRegFake(t)
	p := RegisterParams{Name: "a", Org: "orga", Homepage: "https://example.test", WebBase: "https://github.test",
		APIBase: api.URL, Dir: t.TempDir(), WaitTimeout: 300 * time.Millisecond,
		OnReady: func(r Ready) {
			go func() {
				for _, s := range []string{"WRONG", "", "x"} {
					hit(r.CallbackURL + "?code=thecode&state=" + s)
				}
			}()
		}}
	_, err := Register(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v", err)
	}
	if len(api.paths) != 0 {
		t.Errorf("exchanged a code despite wrong states: %v", api.paths)
	}
}

func TestRegisterMissingCodeRefused(t *testing.T) {
	api := newRegFake(t)
	p := RegisterParams{Name: "a", Org: "orga", Homepage: "https://example.test", WebBase: "https://github.test",
		APIBase: api.URL, Dir: t.TempDir(),
		OnReady: func(r Ready) { go hit(r.CallbackURL + "?state=" + r.State) }}
	if _, err := Register(context.Background(), p); err == nil {
		t.Fatal("missing code accepted")
	}
	if len(api.paths) != 0 {
		t.Error("conversion attempted")
	}
}

func TestRegisterTimesOut(t *testing.T) {
	p := RegisterParams{Name: "a", Org: "orga", Homepage: "https://example.test", WebBase: "https://github.test",
		APIBase: "http://127.0.0.1:1", Dir: t.TempDir(), WaitTimeout: 150 * time.Millisecond}
	start := time.Now()
	_, err := Register(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "timed out") || time.Since(start) > 5*time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
}

func TestRegisterDefaultWaitIs30Min(t *testing.T) {
	if DefaultRegisterWait != 30*time.Minute {
		t.Errorf("wait = %v", DefaultRegisterWait)
	}
}

func TestRegisterValidatesInput(t *testing.T) {
	good := RegisterParams{Name: "a", Org: "orga", Homepage: "https://example.test", Dir: t.TempDir()}
	for name, mut := range map[string]func(*RegisterParams){
		"org":      func(p *RegisterParams) { p.Org = "a/b" },
		"name":     func(p *RegisterParams) { p.Name = "" },
		"homepage": func(p *RegisterParams) { p.Homepage = "" },
	} {
		p := good
		mut(&p)
		if _, err := Register(context.Background(), p); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestRegisterConversionFailureHidesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		w.Write([]byte("PRIVATE-DETAIL"))
	}))
	defer srv.Close()
	p := RegisterParams{Name: "a", Org: "orga", Homepage: "https://example.test", WebBase: "https://github.test",
		APIBase: srv.URL, Dir: t.TempDir(),
		OnReady: func(r Ready) { go hit(r.CallbackURL + "?code=c&state=" + r.State) }}
	_, err := Register(context.Background(), p)
	if err == nil || strings.Contains(err.Error(), "PRIVATE-DETAIL") || !strings.Contains(err.Error(), "422") {
		t.Fatalf("err = %v", err)
	}
}
