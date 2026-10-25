package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/MarimerLLC/claude-utils/internal/ghapp"
)

// appsFileOverride lets tests (and --apps-file) point at another apps.json.
var appsFileOverride string

func appsPath(flagVal string) (string, error) {
	switch {
	case flagVal != "":
		return flagVal, nil
	case appsFileOverride != "":
		return appsFileOverride, nil
	}
	return ghapp.DefaultAppsPath()
}

var (
	minterMu    sync.Mutex
	minterCache = map[string]*ghapp.Minter{}
)

// appMinter returns a process-wide caching minter for the named app, so the
// daemon's hourly re-check reuses one token until it nears expiry. It mints
// only against the entry's own apiBase. org, when non-empty, overrides the
// entry's org.
func appMinter(name, org string) (*ghapp.Minter, ghapp.App, error) {
	path, err := appsPath("")
	if err != nil {
		return nil, ghapp.App{}, err
	}
	a, err := ghapp.LookupApp(path, name)
	if err != nil {
		return nil, a, err
	}
	if org != "" {
		a.Org = org
	}
	k := path + "\x00" + name + "\x00" + a.Org + "\x00" + a.API()
	minterMu.Lock()
	defer minterMu.Unlock()
	if m, ok := minterCache[k]; ok {
		return m, a, nil
	}
	m, err := a.Minter()
	if err != nil {
		return nil, a, err
	}
	minterCache[k] = m
	return m, a, nil
}

// kvList is a repeatable flag of key:value pairs.
type kvList map[string]string

func (l *kvList) String() string { return fmt.Sprint(map[string]string(*l)) }
func (l *kvList) Set(v string) error {
	k, val, ok := strings.Cut(v, ":")
	if !ok || k == "" || (val != "read" && val != "write" && val != "admin") {
		return fmt.Errorf("%q: want <permission>:<read|write|admin>", v)
	}
	if *l == nil {
		*l = kvList{}
	}
	(*l)[k] = val
	return nil
}

// runAppToken handles `claude-memsync app-token --app <name> [--org <org>]`.
func runAppToken(args []string) int { return appTokenCmd(os.Stdout, os.Stderr, args) }

func appTokenCmd(stdout, stderr io.Writer, args []string) int {
	fs := flag.NewFlagSet("app-token", flag.ContinueOnError)
	fs.SetOutput(stderr)
	app := fs.String("app", "", "apps.json entry to mint from (required)")
	org := fs.String("org", "", "org whose installation to use (default: the entry's org)")
	appsFile := fs.String("apps-file", "", "apps.json path (default: ~/.config/claude-memsync/apps.json)")
	var repos stringList
	var perms kvList
	fs.Var(&repos, "repo", "narrow the token to this repository name (repeatable)")
	fs.Var(&perms, "perm", "narrow the token to <permission>:<level>, e.g. metadata:read (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *app == "" {
		fmt.Fprintln(stderr, "app-token: --app is required")
		return 2
	}
	path, err := appsPath(*appsFile)
	if err != nil {
		fmt.Fprintln(stderr, "app-token:", err)
		return 1
	}
	a, err := ghapp.LookupApp(path, *app)
	if err != nil {
		fmt.Fprintln(stderr, "app-token:", err)
		return 1
	}
	if *org != "" {
		a.Org = *org
	}
	m, err := a.Minter()
	if err != nil {
		fmt.Fprintln(stderr, "app-token:", err)
		return 1
	}
	m.Repositories, m.Permissions = repos, perms
	tok, err := m.Token(context.Background())
	if err != nil {
		fmt.Fprintln(stderr, "app-token:", err)
		return 1
	}
	fmt.Fprintln(stdout, tok)
	return 0
}
