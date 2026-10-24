package ghguard

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

// HostResolver maps an ssh host or alias to the real hostname ssh would
// connect to.
type HostResolver func(alias string) (string, error)

// ResolveSSHHost asks `ssh -G` for the effective hostname of alias.
func ResolveSSHHost(alias string) (string, error) {
	if alias == "" || strings.HasPrefix(alias, "-") || strings.ContainsAny(alias, " \t\n") {
		return "", errors.New("unsafe ssh host")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ssh", "-G", alias).Output()
	if err != nil {
		return "", fmt.Errorf("cannot resolve ssh host %q", alias)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) == 2 && strings.EqualFold(f[0], "hostname") {
			return strings.ToLower(f[1]), nil
		}
	}
	return "", fmt.Errorf("ssh -G gave no hostname for %q", alias)
}

// AllowedHost is the git host remotes must use: the explicit host, else the
// host of the API URL (github.com for the public API), else github.com.
func AllowedHost(host, apiURL string) string {
	if host != "" {
		return strings.ToLower(host)
	}
	if apiURL != "" {
		if u, err := url.Parse(apiURL); err == nil && u.Hostname() != "" {
			h := strings.ToLower(u.Hostname())
			if h == "api.github.com" {
				return "github.com"
			}
			return strings.TrimPrefix(h, "api.")
		}
	}
	return "github.com"
}

// ValidateHostAPI ties the git host to the API: github.com with
// api.github.com, or another host with https://<host>/api/v3 or
// https://api.<host>. Any other pairing is refused, so a hostile githubHost
// cannot make a non-GitHub host look allowed.
func ValidateHostAPI(host, apiURL string) error {
	host = strings.ToLower(host)
	bad := fmt.Errorf("git host %q does not match the API URL; use github.com with api.github.com, or https://<host>/api/v3 or https://api.<host>", host)
	if apiURL == "" {
		if host == "github.com" {
			return nil
		}
		return bad
	}
	u, err := url.Parse(apiURL)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return bad
	}
	path := strings.TrimRight(u.Path, "/")
	h := strings.ToLower(u.Hostname())
	switch {
	case host == "github.com":
		if h == "api.github.com" && path == "" {
			return nil
		}
	case h == host && path == "/api/v3":
		return nil
	case h == "api."+host && path == "":
		return nil
	}
	return bad
}

func (c *Client) allowedHost() string { return AllowedHost(c.Host, c.BaseURL) }

// ValidateRemote parses remote and requires a mem-* repo name on the allowed
// git host. https must name the host directly; ssh forms (scp-style, ssh://)
// are resolved with `ssh -G` so an alias cannot point elsewhere. It does no
// network I/O to GitHub.
func ValidateRemote(remote, allowed string, resolve HostResolver) (owner, name string, err error) {
	owner, name, err = ParseRemote(remote)
	if err != nil {
		return "", "", err
	}
	if err := ValidName(name); err != nil {
		return "", "", err
	}
	allowed = strings.ToLower(allowed)
	host, scheme, _, err := splitRemote(remote)
	if err != nil {
		return "", "", err
	}
	viaSSH := scheme != "https"
	if viaSSH {
		if resolve == nil {
			resolve = ResolveSSHHost
		}
		real, rerr := resolve(host)
		if rerr != nil {
			return "", "", fmt.Errorf("cannot resolve ssh host: %w", rerr)
		}
		host = strings.ToLower(real)
		if host == "ssh."+allowed {
			host = allowed
		}
	}
	if host != allowed {
		return "", "", fmt.Errorf("remote host %q is not the allowed git host %q", host, allowed)
	}
	return owner, name, nil
}
