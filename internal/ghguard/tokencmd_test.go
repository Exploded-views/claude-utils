package ghguard

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeCmd(t *testing.T, body string) []string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tok.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	return []string{p}
}

func TestRunTokenCmdOK(t *testing.T) {
	tok, err := RunTokenCmd(fakeCmd(t, "echo ghp_secretvalue123"), time.Second)
	if err != nil || tok != "ghp_secretvalue123" {
		t.Fatalf("got %q %v", tok, err)
	}
}

func TestRunTokenCmdFailureHidesOutput(t *testing.T) {
	_, err := RunTokenCmd(fakeCmd(t, "echo ghp_leakme_out; echo ghp_leakme_err >&2; exit 3"), time.Second)
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "leakme") {
		t.Fatalf("error leaks command output: %v", err)
	}
}

func TestRunTokenCmdEmptyAndMissing(t *testing.T) {
	if _, err := RunTokenCmd(fakeCmd(t, "true"), time.Second); err == nil {
		t.Fatal("empty output must be an error")
	}
	if _, err := RunTokenCmd([]string{"/nonexistent/zzz"}, time.Second); err == nil {
		t.Fatal("missing binary must be an error")
	}
	if _, err := RunTokenCmd(nil, time.Second); err == nil {
		t.Fatal("empty argv must be an error")
	}
}

func TestRunTokenCmdTimeout(t *testing.T) {
	start := time.Now()
	_, err := RunTokenCmd(fakeCmd(t, "exec sleep 5"), 200*time.Millisecond)
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("timeout not enforced: %v after %v", err, time.Since(start))
	}
}

func TestRunTokenCmdNoShell(t *testing.T) {
	// a shell metacharacter in an argument is passed literally
	_, err := RunTokenCmd([]string{"echo", "x;touch /tmp/never"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
}

func TestClientTokenFnPerRequestAndScrub(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		w.WriteHeader(200)
		w.Write([]byte(`{"name":"mem-x","private":true,"visibility":"private"}`))
	}))
	defer srv.Close()
	n := 0
	c := &Client{BaseURL: srv.URL, TokenFn: func() string { n++; return "tok" + string(rune('0'+n)) }}
	for i := 0; i < 2; i++ {
		if _, err := c.GetRepo("o", "mem-x"); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 2 || seen[0] != "Bearer tok1" || seen[1] != "Bearer tok2" {
		t.Fatalf("token not fetched per request: %v", seen)
	}
	if c.Token != "" {
		t.Fatal("token must not be retained on the client")
	}
}

func TestClientTokenFnScrubsErrors(t *testing.T) {
	c := &Client{BaseURL: "http://127.0.0.1:1/ghp_inurl", TokenFn: func() string { return "ghp_inurl" }}
	_, err := c.GetRepo("o", "mem-x")
	if err == nil || strings.Contains(err.Error(), "ghp_inurl") {
		t.Fatalf("error must scrub token: %v", err)
	}
}
