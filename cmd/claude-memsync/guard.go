package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/ghguard"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/hosts"
)

// guardClient builds an API client from config. A nil client with a nil error
// means no token is configured.
func guardClient(cfg config.Config) (*ghguard.Client, error) {
	if err := checkAPIHost(cfg); err != nil {
		return nil, err
	}
	tok, err := ghguard.LoadToken(cfg.GitHubTokenEnv, cfg.GitHubTokenFile)
	if err != nil {
		return nil, err
	}
	if tok == "" {
		return nil, nil
	}
	return &ghguard.Client{BaseURL: cfg.GitHubAPIURL, Token: tok, Host: cfg.GitHubHost, Resolve: hostResolver}, nil
}

// hostResolver maps ssh aliases to real hostnames; nil means `ssh -G`. Tests
// replace it so they need no ssh.
var hostResolver ghguard.HostResolver

func allowedHost(cfg config.Config) string {
	return ghguard.AllowedHost(cfg.GitHubHost, cfg.GitHubAPIURL)
}

// validateRemoteOffline needs no token or network: a mem-* name on the allowed
// git host.
func validateRemoteOffline(cfg config.Config) error {
	if err := checkAPIHost(cfg); err != nil {
		return err
	}
	if err := checkSSHOverride(cfg); err != nil {
		return err
	}
	_, _, err := ghguard.ValidateRemote(cfg.RemoteURL, allowedHost(cfg), hostResolver)
	if err != nil {
		return fmt.Errorf("remote refused: %w", err)
	}
	return nil
}

// skipAPIHostCheck lets tests point the API at httptest.
var skipAPIHostCheck bool

func checkAPIHost(cfg config.Config) error {
	if skipAPIHostCheck {
		return nil
	}
	return ghguard.ValidateHostAPI(allowedHost(cfg), cfg.GitHubAPIURL)
}

// checkSSHOverride refuses when something other than ssh's own config decides
// how git connects, because `ssh -G` would not see it.
func checkSSHOverride(cfg config.Config) error {
	if strings.HasPrefix(strings.TrimSpace(cfg.RemoteURL), "https://") {
		return nil
	}
	if os.Getenv("GIT_SSH_COMMAND") != "" || os.Getenv("GIT_SSH") != "" {
		return errors.New("GIT_SSH_COMMAND or GIT_SSH is set; the ssh host check cannot see it, so private-only mode refuses")
	}
	cmd := exec.Command("git", "config", "--get", "core.sshCommand")
	if st, err := os.Stat(cfg.SyncDir); err == nil && st.IsDir() {
		cmd.Dir = cfg.SyncDir
	}
	if out, err := cmd.Output(); err == nil && strings.TrimSpace(string(out)) != "" {
		return errors.New("git config core.sshCommand is set; the ssh host check cannot see it, so private-only mode refuses")
	}
	return nil
}

func normURL(s string) string {
	s = strings.TrimRight(strings.TrimSpace(s), "/")
	return strings.ToLower(strings.TrimSuffix(s, ".git"))
}

// checkPushURL refuses when git would push somewhere other than cfg.RemoteURL
// (insteadOf / pushInsteadOf rewrites, or a changed origin).
func checkPushURL(cfg config.Config, repoDir string) error {
	out, err := gitwt.New(repoDir).Run("remote", "get-url", "--push", "origin")
	if err != nil {
		return fmt.Errorf("cannot read the push URL: %w", err)
	}
	if normURL(out) != normURL(cfg.RemoteURL) {
		return errors.New("git would push to a different URL than the configured remote (an insteadOf or pushInsteadOf rule?); refusing")
	}
	return nil
}

var errNoToken = errors.New("no GitHub token configured (set " + ghguard.DefaultTokenEnv + " or githubTokenFile); a deploy key cannot call the API, so the private-only check needs one")

// guardRemote is the check run by init before anything is cloned or pushed.
// It fails closed, including when no token is available.
func guardRemote(cfg config.Config) error {
	if !cfg.PrivateOnly {
		return nil
	}
	if err := validateRemoteOffline(cfg); err != nil {
		return err
	}
	c, err := guardClient(cfg)
	if err != nil {
		return err
	}
	if c == nil {
		return errNoToken
	}
	return c.CheckRemote(cfg.RemoteURL)
}

// daemonGuard runs at daemon start. With a token it must pass; without one it
// logs once that the check only ran at init/doctor and lets the daemon start.
func daemonGuard(cfg config.Config, logf func(format string, args ...any)) error {
	if !cfg.PrivateOnly {
		return nil
	}
	if err := validateRemoteOffline(cfg); err != nil {
		return err
	}
	if err := checkPushURL(cfg, cfg.SyncDir); err != nil {
		return err
	}
	c, err := guardClient(cfg)
	if err != nil {
		return err
	}
	if c == nil {
		logf("private-only check skipped: no GitHub token configured; visibility was only verified at init and by `claude-memsync doctor`")
		return nil
	}
	return c.CheckRemote(cfg.RemoteURL)
}

func orgFor(cfg config.Config, flagOrg string) (string, error) {
	if flagOrg != "" {
		return flagOrg, nil
	}
	if cfg.GitHubOrg != "" {
		return cfg.GitHubOrg, nil
	}
	if cfg.RemoteURL != "" {
		if o, _, err := ghguard.ParseRemote(cfg.RemoteURL); err == nil {
			return o, nil
		}
	}
	return "", errors.New("no org: pass --org")
}

// doctorRun lists the org's repos and the repos this host joined. It prints
// repo names only and returns non-zero if any problem is found.
func doctorRun(cfg config.Config, org string, out, errw io.Writer) int {
	var joined []string
	if cfg.RemoteURL != "" {
		joined = append(joined, cfg.RemoteURL)
	}
	return doctorRunJoined(cfg, org, joined, out, errw)
}

// doctorRunJoined is doctorRun over an explicit list of joined remotes: with
// several routed repos, every one of them is checked, not only cfg's.
func doctorRunJoined(cfg config.Config, org string, joined []string, out, errw io.Writer) int {
	c, err := guardClient(cfg)
	if err != nil {
		fmt.Fprintln(errw, "doctor:", err)
		return 1
	}
	if c == nil {
		fmt.Fprintln(errw, "doctor:", errNoToken)
		return 1
	}
	org, err = orgFor(cfg, org)
	if err != nil {
		fmt.Fprintln(errw, "doctor:", err)
		return 2
	}
	bad, err := c.Doctor(org, joined)
	if err != nil {
		fmt.Fprintln(errw, "doctor:", err)
		return 1
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		for _, n := range bad {
			fmt.Fprintf(errw, "NOT PRIVATE OR NOT ALLOWED: %s\n", n)
		}
		return 1
	}
	fmt.Fprintf(out, "ok: every mem-* repo in %s is private\n", org)
	return 0
}

func loadCfgOptional(path string) config.Config {
	if path == "" {
		path = defaultConfigPath()
	}
	if c, err := config.Load(path); err == nil {
		return c
	}
	return config.Defaults()
}

// runDoctor handles `claude-memsync doctor`.
func runDoctor(args []string) int { return doctorCmd(os.Stdout, os.Stderr, args) }

func doctorCmd(stdout, stderr io.Writer, args []string) int {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	org := flags.String("org", "", "GitHub org to check (default: config githubOrg, else owner of the remote)")
	cfgPath := flags.String("config", "", "path to config.json")
	tokenFile := flags.String("token-file", "", "file holding a GitHub token for this run (as for init; or set MEMSYNC_GITHUB_TOKEN); not saved")
	routes := flags.String("routes", "", "path to routes.json: check every repo it routes (default: as for run, unless --config is given)")
	timer := flags.Bool("install-timer", false, "write a daily user timer (systemd) or launchd agent for doctor; does not enable it")
	unitDir := flags.String("unit-dir", "", "with --install-timer: directory to write units into")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	cfg := loadCfgOptional(*cfgPath)
	if *tokenFile != "" {
		if abs, err := filepath.Abs(*tokenFile); err == nil {
			*tokenFile = abs // a timer unit must not depend on the working directory
		}
		cfg.GitHubTokenFile = *tokenFile
	}
	if *timer {
		o, err := orgFor(cfg, *org)
		if err != nil {
			fmt.Fprintln(stderr, "doctor:", err)
			return 2
		}
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintln(stderr, "doctor:", err)
			return 1
		}
		cp := *cfgPath
		if cp == "" {
			cp = defaultConfigPath()
		}
		if abs, err := filepath.Abs(cp); err == nil {
			cp = abs
		}
		return installTimerRun(runtime.GOOS, exe, o, cp, cfg.GitHubTokenFile, *unitDir, stdout, stderr)
	}
	if *cfgPath == "" {
		if path, ok, err := resolveRoutesPath(*routes); err != nil {
			fmt.Fprintln(stderr, "doctor:", err)
			return 1
		} else if ok {
			set, err := loadRouted(path)
			if err != nil {
				fmt.Fprintln(stderr, "doctor:", err)
				return 1
			}
			if *tokenFile != "" {
				for n, c := range set.Cfgs {
					c.GitHubTokenFile = *tokenFile
					set.Cfgs[n] = c
				}
			}
			return doctorWithHosts(routedDoctor(set, *org, stdout, stderr), repoSel{Routes: *routes}, *tokenFile, *org, stdout, stderr)
		}
	}
	return doctorWithHosts(doctorRun(cfg, *org, stdout, stderr), repoSel{Config: *cfgPath}, *tokenFile, *org, stdout, stderr)
}

// doctorWithHosts adds the host lifecycle lines to a doctor run that passed: a
// one-line summary of stale and expired hosts, and the check-key capacity
// warning. Both are silent when there is nothing to say. A check that cannot
// be made (the token cannot read deploy keys) is noted on stderr and never
// changes the exit code.
func doctorWithHosts(code int, sel repoSel, tokenFile, org string, stdout, stderr io.Writer) int {
	if code != 0 {
		return code
	}
	hc, err := loadHostCtx(sel)
	if err != nil {
		return code
	}
	if tokenFile != "" {
		hc.cfg.GitHubTokenFile = tokenFile
	}
	o, err := hc.orgName(org)
	if err != nil {
		return code
	}
	c, err := hc.client()
	if err != nil {
		return code
	}
	rep, err := hc.buildReport(c, o, hosts.DefaultStaleDays, hc.cfg.CheckKeyWarn)
	if err != nil {
		fmt.Fprintln(stderr, "doctor: hosts not checked:", err)
		return code
	}
	if s := rep.Summary(); s != "" {
		fmt.Fprintln(stdout, s)
	}
	for _, w := range rep.Warnings() {
		fmt.Fprintln(stdout, "warning:", w)
	}
	return code
}

// installTimerRun writes the generated unit files. It never enables or starts
// anything; it prints the command for the user to run.
func installTimerRun(goos, exe, org, cfgPath, tokenFile, dir string, out, errw io.Writer) int {
	if tokenFile == "" {
		fmt.Fprintln(errw, "doctor: the timer runs without your environment, so it needs githubTokenFile set in config.json (a file holding the token); refusing to install")
		return 1
	}
	home, _ := os.UserHomeDir()
	if dir == "" {
		switch goos {
		case "linux":
			dir = filepath.Join(home, ".config", "systemd", "user")
		case "darwin":
			dir = filepath.Join(home, "Library", "LaunchAgents")
		default:
			fmt.Fprintln(errw, "doctor: --install-timer supports linux and darwin only")
			return 1
		}
	}
	units, err := ghguard.TimerUnits(goos, exe, org, home, cfgPath)
	if err != nil {
		fmt.Fprintln(errw, "doctor:", err)
		return 1
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		fmt.Fprintln(errw, "doctor:", err)
		return 1
	}
	for name, text := range units {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0644); err != nil {
			fmt.Fprintln(errw, "doctor:", err)
			return 1
		}
		fmt.Fprintln(out, "wrote", filepath.Join(dir, name))
	}
	if goos == "linux" {
		fmt.Fprintln(out, "enable with: systemctl --user daemon-reload && systemctl --user enable --now claude-memsync-doctor.timer")
	} else {
		fmt.Fprintln(out, "enable with: launchctl load "+filepath.Join(dir, "claude-memsync-doctor.plist"))
	}
	return 0
}

// repoCreateRun creates a private mem-* repo and verifies it by reading it back.
func repoCreateRun(cfg config.Config, org, name string, out, errw io.Writer) int {
	if err := ghguard.ValidName(name); err != nil {
		fmt.Fprintln(errw, "repo create:", err)
		return 1
	}
	c, err := guardClient(cfg)
	if err != nil {
		fmt.Fprintln(errw, "repo create:", err)
		return 1
	}
	if c == nil {
		fmt.Fprintln(errw, "repo create:", errNoToken)
		return 1
	}
	org, err = orgFor(cfg, org)
	if err != nil {
		fmt.Fprintln(errw, "repo create:", err)
		return 2
	}
	if err := c.CreateRepo(org, name); err != nil {
		fmt.Fprintln(errw, "repo create:", err)
		return 1
	}
	fmt.Fprintf(out, "created %s/%s and verified it is private\n", org, name)
	return 0
}

// runRepo handles `claude-memsync repo <create> ...`.
func runRepo(args []string) int {
	if len(args) < 1 || args[0] != "create" {
		fmt.Fprintln(os.Stderr, "usage: claude-memsync repo create [--org <org>] [--config <path>] <name>")
		return 2
	}
	flags := flag.NewFlagSet("repo create", flag.ContinueOnError)
	org := flags.String("org", "", "GitHub org to create the repo in")
	cfgPath := flags.String("config", "", "path to config.json")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: claude-memsync repo create [--org <org>] [--config <path>] <name>")
		return 2
	}
	return repoCreateRun(loadCfgOptional(*cfgPath), *org, flags.Arg(0), os.Stdout, os.Stderr)
}

// pushWatch returns the loop's PushAllowed hook, or nil when private-only is
// off or no token is configured (nothing to re-check with).
func pushWatch(cfg config.Config, logf func(string, ...any)) func() bool {
	if !cfg.PrivateOnly {
		return nil
	}
	pre := func() error {
		if err := checkSSHOverride(cfg); err != nil {
			return err
		}
		return checkPushURL(cfg, cfg.SyncDir)
	}
	c, err := guardClient(cfg)
	owner, name, perr := ghguard.ParseRemote(cfg.RemoteURL)
	if err != nil || c == nil || perr != nil {
		// No token: nothing to re-check against the API, but the local
		// checks still run before every push.
		var mu sync.Mutex
		bad := false
		return func() bool {
			e := pre()
			mu.Lock()
			defer mu.Unlock()
			if e != nil && !bad {
				logf("pushing stopped: %v", e)
			} else if e == nil && bad {
				logf("push target check passes again")
			}
			bad = e != nil
			return e == nil
		}
	}
	w := &ghguard.Watch{C: c, Owner: owner, Name: name, Precheck: pre,
		MaxFailures: cfg.RecheckMaxFailures, MaxAge: time.Duration(cfg.RecheckMaxAgeHours) * time.Hour,
		StateDir: filepath.Join(cfg.SyncDir, ".state"), Logf: logf}
	return w.Allowed
}

// visibilityLine is the status line for a repo that stopped being private.
func visibilityLine(cfg config.Config) string {
	if !cfg.PrivateOnly {
		return ""
	}
	if s := ghguard.ReadState(filepath.Join(cfg.SyncDir, ".state")); strings.HasPrefix(s, "not private") || strings.HasPrefix(s, "unverifiable") {
		return "pushing stopped: repo " + s + "; commits stay local"
	}
	return ""
}

// routedDoctor runs doctor over every routed repo with that repo's own org (or
// --org) and its own API settings, so repos spanning orgs are each listed in
// their own org. Repos sharing an org and API are grouped into one run.
func routedDoctor(set *routedSet, flagOrg string, out, errw io.Writer) int {
	type group struct {
		cfg    config.Config
		org    string
		joined []string
	}
	var order []string
	groups := map[string]*group{}
	for _, name := range set.names() {
		cfg := set.Cfgs[name]
		org, err := orgFor(cfg, flagOrg)
		if err != nil {
			fmt.Fprintf(errw, "doctor: repo %q: %v\n", name, err)
			return 2
		}
		key := strings.ToLower(org) + "|" + cfg.GitHubAPIURL + "|" + cfg.GitHubHost
		g := groups[key]
		if g == nil {
			g = &group{cfg: cfg, org: org}
			groups[key] = g
			order = append(order, key)
		}
		if cfg.RemoteURL != "" {
			g.joined = append(g.joined, cfg.RemoteURL)
		}
	}
	code := 0
	for _, k := range order {
		g := groups[k]
		if c := doctorRunJoined(g.cfg, g.org, g.joined, out, errw); c > code {
			code = c
		}
	}
	return code
}
