package ghguard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type fixedSource string

func (f fixedSource) Token(context.Context) (string, error) { return string(f), nil }

type sourceFunc func() (string, error)

func (f sourceFunc) Token(context.Context) (string, error) { return f() }

func TestEnvAndFileSources(t *testing.T) {
	t.Setenv("X_TOK", "  env-tok \n")
	if got, err := (EnvSource{Name: "X_TOK"}).Token(context.Background()); err != nil || got != "env-tok" {
		t.Fatalf("env: %q %v", got, err)
	}
	t.Setenv(DefaultTokenEnv, "dflt")
	if got, _ := (EnvSource{}).Token(context.Background()); got != "dflt" {
		t.Errorf("default env: %q", got)
	}
	p := filepath.Join(t.TempDir(), "t")
	os.WriteFile(p, []byte("file-tok\n"), 0o600)
	if got, err := (FileSource{Path: p}).Token(context.Background()); err != nil || got != "file-tok" {
		t.Fatalf("file: %q %v", got, err)
	}
	if _, err := (FileSource{Path: p + ".missing"}).Token(context.Background()); err == nil {
		t.Error("missing file accepted")
	}
}

func TestCmdSource(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs printf")
	}
	got, err := (CmdSource{Argv: []string{"printf", "cmd-tok\n"}}).Token(context.Background())
	if err != nil || got != "cmd-tok" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := (CmdSource{Argv: []string{"false"}}).Token(context.Background()); err == nil {
		t.Error("failing cmd accepted")
	}
}

func TestSelectSourcePrecedence(t *testing.T) {
	ctx := context.Background()
	t.Setenv("PREC_ENV", "from-env")
	p := filepath.Join(t.TempDir(), "t")
	os.WriteFile(p, []byte("from-file"), 0o600)
	cmd := []string{"printf", "from-cmd"}
	tok := func(s TokenSource) string { v, _ := s.Token(ctx); return v }
	if got := tok(SelectSource(fixedSource("from-app"), p, cmd, "PREC_ENV")); got != "from-app" {
		t.Errorf("app should win, got %q", got)
	}
	if got := tok(SelectSource(nil, p, cmd, "PREC_ENV")); got != "from-file" {
		t.Errorf("file should beat cmd, got %q", got)
	}
	if runtime.GOOS != "windows" {
		if got := tok(SelectSource(nil, "", cmd, "PREC_ENV")); got != "from-cmd" {
			t.Errorf("cmd should beat env, got %q", got)
		}
	}
	if got := tok(SelectSource(nil, "", nil, "PREC_ENV")); got != "from-env" {
		t.Errorf("env last, got %q", got)
	}
}

func TestClientSourceRefreshesPerRequest(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		w.Write([]byte(`{"name":"r","private":true,"visibility":"private"}`))
	}))
	defer srv.Close()
	n := 0
	c := &Client{BaseURL: srv.URL, Source: sourceFunc(func() (string, error) { n++; return "tok-" + string(rune('0'+n)), nil })}
	c.GetRepo("o", "r")
	c.GetRepo("o", "r")
	if len(seen) != 2 || seen[0] != "Bearer tok-1" || seen[1] != "Bearer tok-2" {
		t.Errorf("auth headers = %v", seen)
	}
}

func TestClientSourceErrorStopsTheRequest(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Source: sourceFunc(func() (string, error) { return "", os.ErrPermission })}
	_, err := c.GetRepo("o", "r")
	if err == nil || !strings.Contains(err.Error(), "token source") {
		t.Fatalf("err = %v", err)
	}
	if hits != 0 {
		t.Error("request sent without a token")
	}
}
