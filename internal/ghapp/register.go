package ghapp

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultRegisterWait is how long Register waits for the browser callback.
const DefaultRegisterWait = 30 * time.Minute

// DefaultWebBase is where the manifest form is posted.
const DefaultWebBase = "https://github.com"

// Manifest is a GitHub App manifest (the registration payload).
type Manifest struct {
	Name               string            `json:"name"`
	URL                string            `json:"url"`
	HookAttributes     HookAttributes    `json:"hook_attributes"`
	RedirectURL        string            `json:"redirect_url"`
	Public             bool              `json:"public"`
	DefaultPermissions map[string]string `json:"default_permissions"`
	DefaultEvents      []string          `json:"default_events"`
}

// HookAttributes configures the (disabled) webhook.
type HookAttributes struct {
	URL    string `json:"url"`
	Active bool   `json:"active"`
}

// BuildManifest returns a private app with no webhook and no event
// subscriptions.
func BuildManifest(name, homepage, redirect string, perms map[string]string) Manifest {
	return Manifest{
		Name:               name,
		URL:                homepage,
		HookAttributes:     HookAttributes{URL: "https://example.invalid", Active: false},
		RedirectURL:        redirect,
		Public:             false,
		DefaultPermissions: perms,
		DefaultEvents:      []string{},
	}
}

// ParsePerms turns "name:level" strings into a permissions map.
func ParsePerms(in []string) (map[string]string, error) {
	out := map[string]string{}
	for _, s := range in {
		k, v, ok := strings.Cut(s, ":")
		if !ok || k == "" || (v != "read" && v != "write" && v != "admin") {
			return nil, fmt.Errorf("permission %q: want <name>:<read|write|admin>", s)
		}
		out[k] = v
	}
	return out, nil
}

// Ready describes what the user must open.
type Ready struct {
	FormPath    string // local HTML file to open in a browser
	FileURL     string // file:// URL of FormPath
	CallbackURL string
	State       string
}

// RegisterParams configures Register.
type RegisterParams struct {
	Name     string
	Org      string
	Homepage string // the app's homepage URL (required)
	Perms    map[string]string

	WebBase string // default DefaultWebBase
	APIBase string // default DefaultAPIBase; the conversion is posted here
	Dir     string // where the form file goes (a private temp dir when empty)

	WaitTimeout time.Duration // default DefaultRegisterWait
	HTTP        *http.Client

	// OnReady is called once the listener is up and the form file is written.
	OnReady func(Ready)
}

// Conversion is the result of exchanging the manifest code. PEM is the app's
// private key: write it somewhere safe, overwrite it when done (best effort:
// the response body and decoded strings it was copied from stay in memory
// until the garbage collector reuses them) and never print it.
type Conversion struct {
	ID   int64
	Slug string
	PEM  []byte
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

const formTmpl = `<!doctype html>
<html><head><meta charset="utf-8"><title>Register GitHub App</title></head>
<body>
<p>Submitting the app manifest to GitHub. If nothing happens, press the button.</p>
<form id="f" action="%s" method="post">
<input type="hidden" name="manifest" value="%s">
<input type="hidden" name="state" value="%s">
<button type="submit">Continue to GitHub</button>
</form>
<script>document.getElementById("f").submit();</script>
</body></html>
`

func fileURL(p string) string {
	p = filepath.ToSlash(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // Windows drive paths
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}

// Register runs the GitHub App manifest flow: it serves a loopback callback,
// writes a local auto-submitting form for the user to open, verifies the state
// on the callback, and exchanges the one-time code for the app's credentials.
// Callbacks with a wrong state are refused and ignored; Register keeps waiting
// until a correct one arrives or the wait runs out. It does not contact GitHub
// until the code arrives, and never opens a browser.
func Register(ctx context.Context, p RegisterParams) (Conversion, error) {
	switch {
	case !ValidOrg(p.Org):
		return Conversion{}, errors.New("invalid org name")
	case strings.TrimSpace(p.Name) == "":
		return Conversion{}, errors.New("app name is empty")
	case p.Homepage == "":
		return Conversion{}, errors.New("homepage URL is empty")
	}
	web := strings.TrimRight(p.WebBase, "/")
	if web == "" {
		web = DefaultWebBase
	}
	api := strings.TrimRight(p.APIBase, "/")
	if api == "" {
		api = DefaultAPIBase
	}
	wait := p.WaitTimeout
	if wait <= 0 {
		wait = DefaultRegisterWait
	}
	state, err := randHex(16)
	if err != nil {
		return Conversion{}, errors.New("no randomness available")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return Conversion{}, errors.New("cannot listen on the loopback interface")
	}
	callback := fmt.Sprintf("http://%s/callback", ln.Addr().String())

	type result struct {
		code string
		err  error
	}
	resCh := make(chan result, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
			// Anything on this machine can hit the port; a request with the
			// wrong state is noise (or an attack), not the answer. Refuse it
			// and keep waiting for the real callback.
			http.Error(w, "refused", http.StatusBadRequest)
			return
		}
		var res result
		if q.Get("code") == "" {
			res.err = errors.New("callback had no code; refusing")
			http.Error(w, "refused", http.StatusBadRequest)
		} else {
			res.code = q.Get("code")
			fmt.Fprintln(w, "App registered. You can close this tab and return to the terminal.")
		}
		select {
		case resCh <- res:
		default:
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	defer srv.Close()

	manifest, _ := json.Marshal(BuildManifest(p.Name, p.Homepage, callback, p.Perms))
	action := web + "/organizations/" + url.PathEscape(p.Org) + "/settings/apps/new?state=" + url.QueryEscape(state)
	dir := p.Dir
	if dir == "" {
		if dir, err = os.MkdirTemp("", "memsync-app-"); err != nil {
			return Conversion{}, errors.New("cannot create a temporary directory")
		}
	}
	formPath := filepath.Join(dir, "register-app.html")
	body := fmt.Sprintf(formTmpl, html.EscapeString(action), html.EscapeString(string(manifest)), html.EscapeString(state))
	if err := os.WriteFile(formPath, []byte(body), 0o600); err != nil {
		return Conversion{}, errors.New("cannot write the form file")
	}
	if abs, err := filepath.Abs(formPath); err == nil {
		formPath = abs
	}
	if p.OnReady != nil {
		p.OnReady(Ready{FormPath: formPath, FileURL: fileURL(formPath), CallbackURL: callback, State: state})
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()
	var code string
	select {
	case res := <-resCh:
		if res.err != nil {
			return Conversion{}, res.err
		}
		code = res.code
	case <-timer.C:
		return Conversion{}, errors.New("timed out waiting for the browser callback")
	case <-ctx.Done():
		return Conversion{}, errors.New("cancelled while waiting for the browser callback")
	}
	srv.Close()

	h := p.HTTP
	if h == nil {
		h = &http.Client{Timeout: 30 * time.Second}
	}
	// The code is valid for an hour; it is exchanged as soon as it arrives.
	req, err := http.NewRequestWithContext(ctx, "POST", api+"/app-manifests/"+url.PathEscape(code)+"/conversions", nil)
	if err != nil {
		return Conversion{}, errors.New("bad conversion URL")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := h.Do(req) // unauthenticated by design
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return Conversion{}, fmt.Errorf("manifest conversion failed: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	defer zero(b)
	if err != nil {
		return Conversion{}, errors.New("manifest conversion: cannot read the response")
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return Conversion{}, fmt.Errorf("manifest conversion failed: HTTP %d", resp.StatusCode)
	}
	var out struct {
		ID   int64  `json:"id"`
		Slug string `json:"slug"`
		PEM  string `json:"pem"`
	}
	if json.Unmarshal(b, &out) != nil || out.ID == 0 || out.Slug == "" || out.PEM == "" {
		return Conversion{}, errors.New("manifest conversion: unreadable response")
	}
	return Conversion{ID: out.ID, Slug: out.Slug, PEM: []byte(out.PEM)}, nil
}
