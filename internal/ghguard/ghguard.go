// Package ghguard enforces that memory repos are private, GitHub-side.
//
// Nothing on GitHub stops an org owner from creating or flipping a repo to
// public, so the tool refuses instead: it only touches repos named mem-*, only
// creates them with private:true and visibility "private" named explicitly,
// and fails closed whenever it cannot prove a repo is private.
//
// A deploy key cannot call the API, so the visibility check needs a token. It
// runs at init, in doctor, and at daemon start when a token is configured.
// The token is only ever sent in the Authorization header and is scrubbed
// from every error this package returns.
package ghguard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// DefaultBaseURL is the public GitHub API.
const DefaultBaseURL = "https://api.github.com"

// DefaultTokenEnv is the env var read when no other is configured.
const DefaultTokenEnv = "MEMSYNC_GITHUB_TOKEN"

var nameRE = regexp.MustCompile(`^mem-(registry|personal|orch-all|(project|infra|client|orch)-[a-z0-9][a-z0-9._-]*)$`)

// ValidName reports whether name is one the tool may create, join or check.
func ValidName(name string) error {
	if !nameRE.MatchString(name) || strings.Contains(name, "..") {
		return fmt.Errorf("repo %q refused: memory repos must be named mem-registry, mem-personal, mem-project-<x>, mem-infra-<x>, mem-client-<x>, mem-orch-<x> or mem-orch-all", name)
	}
	return nil
}

// ParseRemote extracts owner and repo from an ssh, ssh:// or https remote URL.
// Any ssh host works, including per-repo aliases such as gh-mem-personal.
func ParseRemote(remote string) (owner, repo string, err error) {
	s := strings.TrimSpace(remote)
	var path string
	switch {
	case strings.Contains(s, "://"):
		u, perr := url.Parse(s)
		if perr != nil || (u.Scheme != "ssh" && u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return "", "", errors.New("unsupported remote URL")
		}
		path = u.Path
	case strings.Contains(s, "@") && strings.Contains(s, ":") && !strings.HasPrefix(s, "/"):
		path = s[strings.Index(s, ":")+1:]
	default:
		return "", "", errors.New("unsupported remote URL")
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", errors.New("remote URL is not of the form owner/repo")
	}
	return parts[0], parts[1], nil
}

// LoadToken reads a token from the file at path when set, else from the env
// var envName (DefaultTokenEnv when empty). An empty result with a nil error
// means no token is configured. Errors never include token text.
func LoadToken(envName, path string) (string, error) {
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read token file: %w", errors.Unwrap(err))
		}
		return strings.TrimSpace(string(b)), nil
	}
	if envName == "" {
		envName = DefaultTokenEnv
	}
	return strings.TrimSpace(os.Getenv(envName)), nil
}

// Client is a minimal GitHub API client.
type Client struct {
	BaseURL string // defaults to DefaultBaseURL; tests point it at httptest
	Token   string
	HTTP    *http.Client
}

// Repo holds the fields the guard reads. Pointers/empty strings distinguish
// "missing" from "false".
type Repo struct {
	Name       string `json:"name"`
	FullName   string `json:"full_name"`
	Private    *bool  `json:"private"`
	Visibility string `json:"visibility"`
}

func (r Repo) verifyPrivate() error {
	switch {
	case r.Private == nil:
		return errors.New("response lacks the private field")
	case r.Visibility == "":
		return errors.New("response lacks the visibility field")
	case !*r.Private || r.Visibility != "private":
		return fmt.Errorf("repo is not private (private=%v visibility=%s)", *r.Private, r.Visibility)
	}
	return nil
}

func (c *Client) base() string {
	if c.BaseURL == "" {
		return DefaultBaseURL
	}
	return strings.TrimRight(c.BaseURL, "/")
}

func (c *Client) scrub(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if c.Token != "" {
		msg = strings.ReplaceAll(msg, c.Token, "[token]")
	}
	return errors.New(msg)
}

// do sends one request and returns status, body and headers. Error bodies are
// deliberately never put into errors.
func (c *Client) do(method, target string, body any) (int, []byte, http.Header, error) {
	return c.doH(method, target, body, "")
}

func (c *Client) doH(method, target string, body any, ifNoneMatch string) (int, []byte, http.Header, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, target, rd)
	if err != nil {
		return 0, nil, nil, c.scrub(err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	h := c.HTTP
	if h == nil {
		h = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := h.Do(req)
	if err != nil {
		return 0, nil, nil, c.scrub(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return 0, nil, nil, c.scrub(err)
	}
	return resp.StatusCode, b, resp.Header, nil
}

// GetRepo fetches one repo.
func (c *Client) GetRepo(owner, name string) (Repo, error) {
	code, b, _, err := c.do("GET", c.base()+"/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name), nil)
	if err != nil {
		return Repo{}, err
	}
	if code != http.StatusOK {
		return Repo{}, fmt.Errorf("GitHub API returned HTTP %d for %s/%s", code, owner, name)
	}
	var r Repo
	if err := json.Unmarshal(b, &r); err != nil {
		return Repo{}, fmt.Errorf("unreadable API response for %s/%s", owner, name)
	}
	return r, nil
}

// CheckPrivate refuses unless owner/name is a mem-* repo that GitHub reports
// as private:true and visibility "private". It fails closed.
func (c *Client) CheckPrivate(owner, name string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	r, err := c.GetRepo(owner, name)
	if err != nil {
		return fmt.Errorf("cannot verify %s/%s is private: %w", owner, name, err)
	}
	if err := r.verifyPrivate(); err != nil {
		return fmt.Errorf("%s/%s: %w", owner, name, err)
	}
	return nil
}

// CheckRemote parses a remote URL and runs CheckPrivate on it.
func (c *Client) CheckRemote(remote string) error {
	owner, name, err := ParseRemote(remote)
	if err != nil {
		return fmt.Errorf("remote refused: %w", err)
	}
	return c.CheckPrivate(owner, name)
}

// CreateRepo creates org/name. It is the only create path in the tool and
// always sends private:true and visibility "private". The result is read back
// and verified before success is reported.
func (c *Client) CreateRepo(org, name string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	req := map[string]any{"name": name, "private": true, "visibility": "private"}
	code, b, _, err := c.do("POST", c.base()+"/orgs/"+url.PathEscape(org)+"/repos", req)
	if err != nil {
		return err
	}
	if code != http.StatusCreated {
		return fmt.Errorf("GitHub API returned HTTP %d creating %s/%s", code, org, name)
	}
	var r Repo
	if err := json.Unmarshal(b, &r); err == nil {
		if err := r.verifyPrivate(); err != nil {
			return fmt.Errorf("created %s/%s but it is not private: %w", org, name, err)
		}
	}
	return c.CheckPrivate(org, name)
}

var nextRE = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="next"`)

// ListOrgRepos returns every repo of org, following Link headers. It never
// follows a next link to a different host than the base URL.
func (c *Client) ListOrgRepos(org string) ([]Repo, error) {
	baseU, err := url.Parse(c.base())
	if err != nil {
		return nil, c.scrub(err)
	}
	next := c.base() + "/orgs/" + url.PathEscape(org) + "/repos?type=all&per_page=100"
	var all []Repo
	for pages := 0; next != ""; pages++ {
		if pages > 1000 {
			return nil, errors.New("too many pages listing org repos")
		}
		code, b, h, err := c.do("GET", next, nil)
		if err != nil {
			return nil, err
		}
		if code != http.StatusOK {
			return nil, fmt.Errorf("GitHub API returned HTTP %d listing %s repos", code, org)
		}
		var page []Repo
		if err := json.Unmarshal(b, &page); err != nil {
			return nil, fmt.Errorf("unreadable API response listing %s repos", org)
		}
		all = append(all, page...)
		next = ""
		if m := nextRE.FindStringSubmatch(h.Get("Link")); m != nil {
			nu, err := url.Parse(m[1])
			if err != nil || nu.Host != baseU.Host || nu.Scheme != baseU.Scheme {
				return nil, errors.New("pagination link points to a different host; refusing to follow")
			}
			next = m[1]
		}
	}
	return all, nil
}

// Doctor lists the org's repos and returns one line per problem: every mem-*
// repo that is not provably private, plus every joined remote that is not a
// private mem-* repo. Public repos without the mem- prefix are ignored. Lines
// name repos only, never contents.
func (c *Client) Doctor(org string, joined []string) ([]string, error) {
	repos, err := c.ListOrgRepos(org)
	if err != nil {
		return nil, err
	}
	var bad []string
	for _, r := range repos {
		if !strings.HasPrefix(r.Name, "mem-") {
			continue
		}
		if err := r.verifyPrivate(); err != nil {
			bad = append(bad, r.Name)
		}
	}
	for _, remote := range joined {
		if err := c.CheckRemote(remote); err != nil {
			if o, n, perr := ParseRemote(remote); perr == nil {
				bad = append(bad, o+"/"+n)
			} else {
				bad = append(bad, "(unparseable joined remote)")
			}
		}
	}
	return bad, nil
}

// TimerUnits returns file name -> text for a daily doctor timer: a systemd
// user service+timer on linux, a launchd plist on darwin. It only generates
// text; the caller decides whether to write it.
func TimerUnits(goos, exe, org, home string) map[string]string {
	if goos == "darwin" {
		const label = "claude-memsync-doctor"
		return map[string]string{label + ".plist": fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>doctor</string>
    <string>--org</string>
    <string>%s</string>
  </array>
  <key>StartCalendarInterval</key>
  <dict><key>Hour</key><integer>9</integer><key>Minute</key><integer>0</integer></dict>
  <key>StandardErrorPath</key><string>%s/.claudesync/doctor.log</string>
  <key>StandardOutPath</key><string>%s/.claudesync/doctor.log</string>
</dict>
</plist>
`, label, exe, org, home, home)}
	}
	return map[string]string{
		"claude-memsync-doctor.service": fmt.Sprintf(`[Unit]
Description=claude-memsync doctor (private-only check)

[Service]
Type=oneshot
ExecStart=%s doctor --org %s
`, exe, org),
		"claude-memsync-doctor.timer": `[Unit]
Description=Daily claude-memsync doctor

[Timer]
OnCalendar=daily
Persistent=true

[Install]
WantedBy=timers.target
`,
	}
}
