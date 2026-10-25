package ghapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// DefaultAPIBase is the public GitHub API.
const DefaultAPIBase = "https://api.github.com"

// refreshMargin: a cached token is replaced once it has less than this left.
const refreshMargin = 5 * time.Minute

// maxTokenLife caps how long a minted token is cached.
const maxTokenLife = time.Hour

var orgRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)

// ValidOrg reports whether s looks like a GitHub org or user login.
func ValidOrg(s string) bool { return orgRE.MatchString(s) }

// Token is an installation access token. Treat Value as a secret.
type Token struct {
	Value     string
	ExpiresAt time.Time
}

// Params is one mint request. Key (PEM) is overwritten with zeros once parsed
// (best effort, see ParseKey).
type Params struct {
	AppID   string
	Key     []byte
	Org     string
	APIBase string // default DefaultAPIBase

	// Repositories and Permissions narrow the token below what the
	// installation grants. Empty means no narrowing.
	Repositories []string
	Permissions  map[string]string

	HTTP *http.Client
	Now  func() time.Time
}

// Mint creates a token for the app's installation on org. pemKey is
// overwritten with zeros once parsed (best effort, see ParseKey).
func Mint(ctx context.Context, appID string, pemKey []byte, org, apiBase string) (Token, error) {
	return MintWith(ctx, Params{AppID: appID, Key: pemKey, Org: org, APIBase: apiBase})
}

// scrubText replaces every secret in msg.
func scrubText(msg string, secrets ...string) string {
	for _, s := range secrets {
		if s != "" {
			msg = strings.ReplaceAll(msg, s, "[redacted]")
		}
	}
	return msg
}

// MintWith is Mint with options.
func MintWith(ctx context.Context, p Params) (Token, error) {
	if p.AppID == "" {
		zero(p.Key)
		return Token{}, errors.New("app id is empty")
	}
	if !ValidOrg(p.Org) {
		zero(p.Key)
		return Token{}, errors.New("invalid org name")
	}
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	base := strings.TrimRight(p.APIBase, "/")
	if base == "" {
		base = DefaultAPIBase
	}
	key, err := ParseKey(p.Key)
	if err != nil {
		return Token{}, err
	}
	jwt, err := signJWT(key, p.AppID, now())
	if err != nil {
		return Token{}, err
	}
	h := p.HTTP
	if h == nil {
		h = &http.Client{Timeout: 30 * time.Second}
	}
	var tok Token
	fail := func(what string, err error) (Token, error) {
		return Token{}, errors.New(scrubText(what+": "+err.Error(), jwt, tok.Value))
	}

	// installation id
	body, code, err := call(ctx, h, "GET", base+"/orgs/"+url.PathEscape(p.Org)+"/installation", jwt, nil)
	if err != nil {
		return fail("installation lookup", err)
	}
	if code != http.StatusOK {
		return fail("installation lookup", fmt.Errorf("GitHub returned HTTP %d (is the app installed on %s?)", code, p.Org))
	}
	var inst struct {
		ID int64 `json:"id"`
	}
	if json.Unmarshal(body, &inst) != nil || inst.ID == 0 {
		return fail("installation lookup", errors.New("unreadable response"))
	}

	// access token
	var req []byte
	if len(p.Repositories) > 0 || len(p.Permissions) > 0 {
		m := map[string]any{}
		if len(p.Repositories) > 0 {
			m["repositories"] = p.Repositories
		}
		if len(p.Permissions) > 0 {
			m["permissions"] = p.Permissions
		}
		req, _ = json.Marshal(m)
	}
	body, code, err = call(ctx, h, "POST", fmt.Sprintf("%s/app/installations/%d/access_tokens", base, inst.ID), jwt, req)
	if err != nil {
		return fail("token request", err)
	}
	if code != http.StatusCreated && code != http.StatusOK {
		return fail("token request", fmt.Errorf("GitHub returned HTTP %d", code))
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if json.Unmarshal(body, &out) != nil || out.Token == "" {
		return fail("token request", errors.New("unreadable response"))
	}
	// Installation tokens last an hour. Whatever the server claims, never
	// trust a token for longer than that, so a bogus expiry cannot keep a
	// revoked or rotated token cached.
	if limit := now().Add(maxTokenLife); out.ExpiresAt.IsZero() || out.ExpiresAt.After(limit) {
		out.ExpiresAt = limit
	}
	return Token{Value: out.Token, ExpiresAt: out.ExpiresAt}, nil
}

// call sends one request authenticated with the JWT. Response bodies are
// returned to the caller only for parsing; they never go into errors.
func call(ctx context.Context, h *http.Client, method, target, jwt string, body []byte) ([]byte, int, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return nil, 0, errors.New("bad request URL")
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // drop the URL, keep the cause
		}
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, 0, errors.New("cannot read the response")
	}
	return b, resp.StatusCode, nil
}

// Minter caches a token in memory and mints a new one when fewer than five
// minutes remain. LoadKey is called for each mint (the key buffer is
// overwritten after parsing, so the key is re-read rather than kept). It is safe for concurrent use
// and implements ghguard.TokenSource.
type Minter struct {
	AppID        string
	Org          string
	APIBase      string
	Repositories []string
	Permissions  map[string]string
	LoadKey      func(ctx context.Context) ([]byte, error)
	HTTP         *http.Client
	Now          func() time.Time

	mu  sync.Mutex
	tok Token
}

// Token returns the cached token or mints a new one.
func (m *Minter) Token(ctx context.Context) (string, error) {
	t, err := m.get(ctx)
	return t.Value, err
}

func (m *Minter) get(ctx context.Context) (Token, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now
	if m.Now != nil {
		now = m.Now
	}
	if m.tok.Value != "" && now().Before(m.tok.ExpiresAt.Add(-refreshMargin)) {
		return m.tok, nil
	}
	key, err := m.LoadKey(ctx)
	if err != nil {
		return Token{}, err
	}
	t, err := MintWith(ctx, Params{AppID: m.AppID, Key: key, Org: m.Org, APIBase: m.APIBase,
		Repositories: m.Repositories, Permissions: m.Permissions, HTTP: m.HTTP, Now: m.Now})
	if err != nil {
		return Token{}, err
	}
	m.tok = t
	return t, nil
}
