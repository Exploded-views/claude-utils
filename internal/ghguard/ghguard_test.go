package ghguard

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

const testToken = "tok-SECRET-1234567890"

type fakeRepo struct {
	Name       string `json:"name"`
	FullName   string `json:"full_name,omitempty"`
	Private    *bool  `json:"private,omitempty"`
	Visibility string `json:"visibility,omitempty"`
}

func bp(b bool) *bool { return &b }

func TestValidName(t *testing.T) {
	good := []string{"mem-registry", "mem-personal", "mem-project-widgets", "mem-infra-prod",
		"mem-client-acme", "mem-orch-acme", "mem-orch-all"}
	for _, n := range good {
		if err := ValidName(n); err != nil {
			t.Errorf("%s should be allowed: %v", n, err)
		}
	}
	bad := []string{"", "widgets", "claude-memories", "mem-", "mem-other", "mem-project-", "Mem-personal",
		"mem-project-a/b", "mem-project-..", "xmem-personal", "mem-personal-extra", "mem-orch-"}
	for _, n := range bad {
		if err := ValidName(n); err == nil {
			t.Errorf("%q should be refused", n)
		}
	}
}

func TestParseRemote(t *testing.T) {
	cases := []struct{ in, owner, repo string }{
		{"git@github.com:Org/mem-personal.git", "Org", "mem-personal"},
		{"git@gh-mem-personal:Org/mem-personal.git", "Org", "mem-personal"},
		{"ssh://git@gh-mem-registry/Org/mem-registry.git", "Org", "mem-registry"},
		{"https://github.com/Org/mem-personal.git", "Org", "mem-personal"},
		{"https://github.com/Org/mem-personal", "Org", "mem-personal"},
		{"https://user:pw@github.com/Org/mem-personal.git", "Org", "mem-personal"},
		{"https://github.com/Org/mem-personal/", "Org", "mem-personal"},
	}
	for _, c := range cases {
		o, r, err := ParseRemote(c.in)
		if err != nil || o != c.owner || r != c.repo {
			t.Errorf("%s: got %q %q %v", c.in, o, r, err)
		}
	}
	for _, in := range []string{"", "/local/path/origin.git", "git@host", "https://github.com/onlyowner", "file:///x/y/z"} {
		if _, _, err := ParseRemote(in); err == nil {
			t.Errorf("%q should not parse", in)
		}
	}
}

func newServer(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, Token: testToken}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func TestCreateRepoRequestIsPrivate(t *testing.T) {
	var got map[string]any
	var path, auth string
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			writeJSON(w, 200, fakeRepo{Name: "mem-personal", Private: bp(true), Visibility: "private"})
			return
		}
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		writeJSON(w, 201, fakeRepo{Name: "mem-personal", Private: bp(true), Visibility: "private"})
	})
	if err := c.CreateRepo("Org", "mem-personal"); err != nil {
		t.Fatal(err)
	}
	if path != "/orgs/Org/repos" {
		t.Errorf("path %s", path)
	}
	if got["private"] != true || got["visibility"] != "private" || got["name"] != "mem-personal" {
		t.Errorf("request body %v", got)
	}
	if auth != "Bearer "+testToken {
		t.Errorf("auth header not sent")
	}
}

func TestCreateRepoRefusesBadNameWithoutRequest(t *testing.T) {
	called := false
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	if err := c.CreateRepo("Org", "widgets"); err == nil {
		t.Fatal("expected refusal")
	}
	if called {
		t.Fatal("no request may be sent for a non-mem name")
	}
}

func TestCheckPrivateRefusals(t *testing.T) {
	cases := map[string]struct {
		code int
		body any
	}{
		"public":     {200, fakeRepo{Name: "mem-personal", Private: bp(false), Visibility: "public"}},
		"internal":   {200, fakeRepo{Name: "mem-personal", Private: bp(true), Visibility: "internal"}},
		"no-private": {200, fakeRepo{Name: "mem-personal", Visibility: "private"}},
		"no-vis":     {200, fakeRepo{Name: "mem-personal", Private: bp(true)}},
		"mismatch":   {200, fakeRepo{Name: "mem-personal", Private: bp(false), Visibility: "private"}},
		"api-error":  {500, map[string]string{"message": "boom"}},
		"not-found":  {404, map[string]string{"message": "Not Found"}},
		"bad-json":   {200, "not an object"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := newServer(t, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, tc.code, tc.body) })
			if err := c.CheckPrivate("Org", "mem-personal"); err == nil {
				t.Fatal("must refuse (fail closed)")
			}
		})
	}
}

func TestCheckPrivateOK(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/Org/mem-personal" {
			t.Errorf("path %s", r.URL.Path)
		}
		writeJSON(w, 200, fakeRepo{Name: "mem-personal", Private: bp(true), Visibility: "private"})
	})
	if err := c.CheckPrivate("Org", "mem-personal"); err != nil {
		t.Fatal(err)
	}
}

func TestCheckPrivateRefusesNonMemWithoutRequest(t *testing.T) {
	called := false
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	if err := c.CheckPrivate("Org", "widgets"); err == nil || called {
		t.Fatalf("err=%v called=%v", err, called)
	}
}

func TestCheckRemote(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, fakeRepo{Name: "mem-personal", Private: bp(false), Visibility: "public"})
	})
	if err := c.CheckRemote("git@gh-mem-personal:Org/mem-personal.git"); err == nil {
		t.Fatal("public must be refused")
	}
	if err := c.CheckRemote("git@gh-widgets:Org/widgets.git"); err == nil {
		t.Fatal("non-mem must be refused")
	}
}

func TestListOrgReposPaginates(t *testing.T) {
	var srvURL string
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "", "1":
			w.Header().Set("Link", fmt.Sprintf(`<%s/orgs/Org/repos?page=2>; rel="next", <%s/orgs/Org/repos?page=3>; rel="last"`, srvURL, srvURL))
			writeJSON(w, 200, []fakeRepo{{Name: "a"}, {Name: "b"}})
		case "2":
			w.Header().Set("Link", fmt.Sprintf(`<%s/orgs/Org/repos?page=3>; rel="next"`, srvURL))
			writeJSON(w, 200, []fakeRepo{{Name: "c"}})
		default:
			writeJSON(w, 200, []fakeRepo{{Name: "d"}})
		}
	})
	srvURL = c.BaseURL
	repos, err := c.ListOrgRepos("Org")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range repos {
		names = append(names, r.Name)
	}
	if strings.Join(names, ",") != "a,b,c,d" {
		t.Errorf("got %v", names)
	}
}

func TestPaginationNeverFollowsForeignHost(t *testing.T) {
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("token-bearing request reached a foreign host")
	}))
	defer foreign.Close()
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", fmt.Sprintf(`<%s/x?page=2>; rel="next"`, foreign.URL))
		writeJSON(w, 200, []fakeRepo{{Name: "a"}})
	})
	if _, err := c.ListOrgRepos("Org"); err == nil {
		t.Fatal("expected error on foreign next link")
	}
}

func TestDoctorFindings(t *testing.T) {
	list := []fakeRepo{
		{Name: "mem-personal", Private: bp(true), Visibility: "private"},
		{Name: "mem-project-x", Private: bp(false), Visibility: "public"},
		{Name: "mem-client-y", Private: bp(true), Visibility: "internal"},
		{Name: "website", Private: bp(false), Visibility: "public"},
	}
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, list) })
	bad, err := c.Doctor("Org", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(bad, ",")
	if got != "mem-project-x,mem-client-y" {
		t.Errorf("findings %q", got)
	}
	if strings.Contains(got, "website") {
		t.Error("non-mem public repo must be ignored")
	}
}

func TestDoctorOnlyNonMemPublicIsClean(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, []fakeRepo{{Name: "website", Private: bp(false), Visibility: "public"},
			{Name: "mem-personal", Private: bp(true), Visibility: "private"}})
	})
	bad, err := c.Doctor("Org", nil)
	if err != nil || len(bad) != 0 {
		t.Fatalf("bad=%v err=%v", bad, err)
	}
}

func TestDoctorMissingFieldCounts(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, []fakeRepo{{Name: "mem-personal"}})
	})
	bad, _ := c.Doctor("Org", nil)
	if len(bad) != 1 {
		t.Fatalf("bad=%v", bad)
	}
}

func TestDoctorJoinedRemotes(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/orgs/Org/repos" {
			writeJSON(w, 200, []fakeRepo{})
			return
		}
		writeJSON(w, 200, fakeRepo{Name: "mem-personal", Private: bp(false), Visibility: "public"})
	})
	bad, err := c.Doctor("Org", []string{"git@gh-mem-personal:Org/mem-personal.git", "git@github.com:Org/website.git"})
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 2 {
		t.Fatalf("want both joined problems, got %v", bad)
	}
}

func TestTokenNeverInErrors(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		// A hostile server echoing the credential back.
		writeJSON(w, 500, map[string]string{"message": "bad credentials " + r.Header.Get("Authorization")})
	})
	errs := []error{c.CheckPrivate("Org", "mem-personal"), c.CreateRepo("Org", "mem-personal")}
	_, e := c.ListOrgRepos("Org")
	errs = append(errs, e)
	for _, err := range errs {
		if err == nil {
			t.Fatal("expected error")
		}
		if strings.Contains(err.Error(), testToken) {
			t.Errorf("token leaked: %v", err)
		}
	}
	// Unreachable server: transport errors must not carry it either.
	c2 := &Client{BaseURL: "http://127.0.0.1:1", Token: testToken}
	if err := c2.CheckPrivate("Org", "mem-personal"); err == nil || strings.Contains(err.Error(), testToken) {
		t.Errorf("err=%v", err)
	}
}

func TestLoadToken(t *testing.T) {
	t.Setenv("MEMSYNC_TEST_TOK", " from-env \n")
	if v, err := LoadToken("MEMSYNC_TEST_TOK", ""); err != nil || v != "from-env" {
		t.Fatalf("%q %v", v, err)
	}
	t.Setenv("MEMSYNC_TEST_TOK", "")
	if v, err := LoadToken("MEMSYNC_TEST_TOK", ""); err != nil || v != "" {
		t.Fatalf("empty env: %q %v", v, err)
	}
	f := t.TempDir() + "/tok"
	if err := writeFile(f, "from-file\n"); err != nil {
		t.Fatal(err)
	}
	if v, err := LoadToken("MEMSYNC_TEST_TOK", f); err != nil || v != "from-file" {
		t.Fatalf("%q %v", v, err)
	}
	if _, err := LoadToken("", t.TempDir()+"/missing"); err == nil || strings.Contains(err.Error(), "from-file") {
		t.Errorf("missing file must error: %v", err)
	}
}

func TestTimerUnitText(t *testing.T) {
	u := TimerUnits("linux", "/opt/bin/claude-memsync", "example-org", "/home/u")
	svc, tim := u["claude-memsync-doctor.service"], u["claude-memsync-doctor.timer"]
	if !strings.Contains(svc, "ExecStart=/opt/bin/claude-memsync doctor --org example-org") {
		t.Errorf("service:\n%s", svc)
	}
	if !strings.Contains(tim, "OnCalendar=daily") || !strings.Contains(tim, "Persistent=true") {
		t.Errorf("timer:\n%s", tim)
	}
	m := TimerUnits("darwin", "/opt/bin/claude-memsync", "example-org", "/Users/u")
	var plist string
	for k, v := range m {
		if strings.HasSuffix(k, ".plist") {
			plist = v
		}
	}
	if !strings.Contains(plist, "StartCalendarInterval") || !strings.Contains(plist, "<string>doctor</string>") || !strings.Contains(plist, "example-org") {
		t.Errorf("plist:\n%s", plist)
	}
	for _, v := range u {
		if strings.Contains(v, "token") && strings.Contains(v, testToken) {
			t.Error("unit must not embed a token")
		}
	}
}

func writeFile(p, s string) error { return os.WriteFile(p, []byte(s), 0600) }

func TestCreateRepoReadBackMustBePrivate(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			writeJSON(w, 201, fakeRepo{Name: "mem-personal", Private: bp(true), Visibility: "private"})
			return
		}
		writeJSON(w, 200, fakeRepo{Name: "mem-personal", Private: bp(false), Visibility: "public"})
	})
	if err := c.CreateRepo("Org", "mem-personal"); err == nil {
		t.Fatal("a read-back that is public must fail the create")
	}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func TestWatchHourlyETagAndStateChanges(t *testing.T) {
	var reqs int
	var inm []string
	public, fail := false, false
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		reqs++
		inm = append(inm, r.Header.Get("If-None-Match"))
		if fail {
			writeJSON(w, 500, map[string]string{})
			return
		}
		if r.Header.Get("If-None-Match") == `"v1"` && !public {
			w.WriteHeader(304)
			return
		}
		if public {
			w.Header().Set("ETag", `"v2"`)
			writeJSON(w, 200, fakeRepo{Name: "mem-personal", Private: bp(false), Visibility: "public"})
			return
		}
		w.Header().Set("ETag", `"v1"`)
		writeJSON(w, 200, fakeRepo{Name: "mem-personal", Private: bp(true), Visibility: "private"})
	})
	dir := t.TempDir()
	clk := &clock{time.Unix(1000000, 0)}
	var logs []string
	w := &Watch{C: c, Owner: "Org", Name: "mem-personal", Now: clk.now, StateDir: dir,
		Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }}

	if !w.Allowed() || reqs != 1 {
		t.Fatalf("first poll: reqs=%d", reqs)
	}
	clk.t = clk.t.Add(59 * time.Minute)
	w.Allowed()
	if reqs != 1 {
		t.Fatalf("must not call API within the hour, reqs=%d", reqs)
	}
	clk.t = clk.t.Add(2 * time.Minute)
	if !w.Allowed() || reqs != 2 || inm[1] != `"v1"` {
		t.Fatalf("304 path: reqs=%d inm=%v", reqs, inm)
	}
	if len(logs) != 0 {
		t.Fatalf("no state change, no log: %v", logs)
	}
	// API failure keeps last state, logs once.
	fail = true
	clk.t = clk.t.Add(61 * time.Minute)
	if !w.Allowed() {
		t.Fatal("API failure must not block")
	}
	clk.t = clk.t.Add(61 * time.Minute)
	w.Allowed()
	if len(logs) != 1 {
		t.Fatalf("failure logged once, got %v", logs)
	}
	fail = false
	// Goes public.
	public = true
	clk.t = clk.t.Add(61 * time.Minute)
	if w.Allowed() {
		t.Fatal("public must stop pushing")
	}
	if len(logs) != 2 {
		t.Fatalf("one line per state change: %v", logs)
	}
	if got := ReadState(dir); !strings.Contains(got, "not private") {
		t.Errorf("state file: %q", got)
	}
	clk.t = clk.t.Add(61 * time.Minute)
	if w.Allowed() || len(logs) != 2 {
		t.Fatalf("still public, no extra log: %v", logs)
	}
	// API failure while public keeps blocking.
	fail = true
	clk.t = clk.t.Add(61 * time.Minute)
	if w.Allowed() {
		t.Fatal("failure must keep last known (not private) state")
	}
	for _, l := range logs {
		if strings.Contains(l, testToken) {
			t.Error("token in log")
		}
	}
}
