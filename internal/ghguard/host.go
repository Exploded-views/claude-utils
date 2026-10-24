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
		if u, err := url.Parse(apiURL); err == nil && u.Hostname() != "" && !strings.EqualFold(u.Hostname(), "api.github.com") {
			return strings.ToLower(u.Hostname())
		}
	}
	return "github.com"
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
	s := strings.TrimSpace(remote)
	var host string
	viaSSH := true
	if strings.Contains(s, "://") {
		u, _ := url.Parse(s)
		host = strings.ToLower(u.Hostname())
		switch u.Scheme {
		case "https":
			viaSSH = false
		case "ssh":
		default:
			return "", "", fmt.Errorf("remote scheme %q refused", u.Scheme)
		}
	} else {
		at := strings.Index(s, "@")
		host = strings.ToLower(s[at+1 : strings.Index(s, ":")])
	}
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
