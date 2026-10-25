package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/ghguard"
	"github.com/MarimerLLC/claude-utils/internal/hosts"
	"github.com/MarimerLLC/claude-utils/internal/route"
)

// localRepo is a mem-* repo this host has: its repo name on GitHub and the
// sync work-tree.
type localRepo struct {
	Name string
	Dir  string
	Cfg  config.Config
}

// hostCtx is what the host commands act on: the API settings, the repos this
// host has, the registry work-tree (if configured) and this host's own labels.
type hostCtx struct {
	cfg         config.Config // API settings (url, token, host, org)
	org         string
	local       []localRepo
	registryDir string
	// registryReadOnly: this host is a read-only member of the registry repo
	// and so never commits into it.
	registryReadOnly bool
	ownLabels        []string
	now              time.Time // zero: time.Now()
}

func (hc *hostCtx) clock() time.Time {
	if hc.now.IsZero() {
		return time.Now()
	}
	return hc.now
}

// repoNameFromRemote is the lower-cased last path element of a remote, minus
// ".git": the repo's name on the host, for ssh, https and local remotes alike.
func repoNameFromRemote(remote string) string {
	base := strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(remote), "/"), ".git")
	if i := strings.LastIndexAny(base, "/:\\"); i >= 0 {
		base = base[i+1:]
	}
	return strings.ToLower(base)
}

// loadHostCtx builds the context from routes.json when this host has one, else
// from the single config.
func loadHostCtx(sel repoSel) (*hostCtx, error) {
	hc := &hostCtx{}
	add := func(name string, cfg config.Config) {
		if cfg.HostLabel != "" {
			hc.ownLabels = append(hc.ownLabels, cfg.HostLabel)
		}
		if n := repoNameFromRemote(cfg.RemoteURL); n != "" {
			hc.local = append(hc.local, localRepo{Name: n, Dir: cfg.SyncDir, Cfg: cfg})
		}
	}
	path, routed, err := resolveRoutesPath(sel.Routes)
	if sel.Config != "" {
		routed = false
		err = nil
	}
	if err != nil {
		return nil, err
	}
	if routed {
		set, err := loadRouted(path)
		if err != nil {
			return nil, err
		}
		primary, ok := set.Primary()
		if !ok {
			return nil, fmt.Errorf("%s: this host has none of the repos", path)
		}
		hc.cfg = primary
		for _, n := range set.names() {
			add(n, set.Cfgs[n])
		}
		if set.File.Registry != "" {
			hc.registryDir = route.ExpandHome(set.File.Registry)
		}
	} else {
		cfgPath := sel.Config
		if cfgPath == "" {
			cfgPath = defaultConfigPath()
		}
		cfg, err := config.Load(cfgPath)
		if err != nil {
			return nil, fmt.Errorf("load config %s: %w", cfgPath, err)
		}
		hc.cfg = cfg
		add("", cfg)
	}
	hc.ownLabels = uniq(hc.ownLabels)
	for _, l := range hc.local {
		if hc.registryDir != "" && sameDir(l.Dir, hc.registryDir) && l.Cfg.ReadOnly {
			hc.registryReadOnly = true
		}
	}
	return hc, nil
}

func sameDir(a, b string) bool {
	ra, err1 := resolvePath(a)
	rb, err2 := resolvePath(b)
	return err1 == nil && err2 == nil && ra == rb
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func (hc *hostCtx) orgName(flagOrg string) (string, error) {
	if flagOrg != "" {
		return flagOrg, nil
	}
	if hc.org != "" {
		return hc.org, nil
	}
	return orgFor(hc.cfg, "")
}

var errHostNoToken = errors.New("no GitHub token: set githubTokenFile in config.json, pass --token-file or set MEMSYNC_GITHUB_TOKEN")

func (hc *hostCtx) client() (*ghguard.Client, error) {
	c, err := guardClient(hc.cfg)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, errHostNoToken
	}
	return c, nil
}

// permissionHint turns a 403/404 from the keys endpoints into what the token
// needs.
func permissionHint(err error, need string) error {
	var se *ghguard.StatusError
	if errors.Is(err, ghguard.ErrForbidden) || (errors.As(err, &se) && (se.Code == http.StatusForbidden || se.Code == http.StatusNotFound)) {
		return fmt.Errorf("%w (the token needs %s on the mem-* repos)", err, need)
	}
	return err
}

// buildReport reads the org's mem-* repos and their deploy keys, this host's
// labelled recipients and the registry, and joins them.
func (hc *hostCtx) buildReport(c *ghguard.Client, org string, staleDays, warnAt int) (hosts.Report, error) {
	repos, err := c.ListMemRepos(org)
	if err != nil {
		return hosts.Report{}, err
	}
	in := hosts.Input{
		Keys:  map[string][]ghguard.DeployKey{},
		Local: map[string][]string{},
		Now:   hc.clock(), StaleDays: staleDays, CheckKeyWarn: warnAt,
	}
	for _, r := range repos {
		ks, err := c.ListDeployKeys(org, r)
		if err != nil {
			return hosts.Report{}, permissionHint(fmt.Errorf("deploy keys of %s: %w", r, err), "Administration: read")
		}
		in.Keys[r] = ks
	}
	for _, l := range hc.local {
		if ghguard.ValidName(l.Name) != nil {
			continue // never look at anything outside mem-*
		}
		labels, _ := agecrypt.ListLabels(l.Dir) // an unencrypted repo has none
		in.Local[l.Name] = labels
	}
	if hc.registryDir != "" {
		reg, err := hosts.ReadRegistry(hc.registryDir)
		if err != nil {
			return hosts.Report{}, err
		}
		in.Registry, in.HaveRegistry = reg, true
	}
	return hosts.Build(in), nil
}

func runHost(args []string) int { return hostCmd(os.Stdout, os.Stderr, args) }

const hostUsage = `usage: claude-memsync host list [--org ORG] [--stale-days N] [--json]
       claude-memsync host retire <label> [--apply] [--org ORG]
       claude-memsync host check-key --fingerprint SHA256:... --app SLUG | --clear
  all take [--config PATH | --routes PATH] [--token-file FILE]`

func hostCmd(stdout, stderr io.Writer, args []string) int {
	sel, rest, err := extractRepoFlags(args)
	if err != nil {
		fmt.Fprintln(stderr, "host:", err)
		return 2
	}
	if len(rest) == 0 || rest[0] == "help" || rest[0] == "--help" {
		fmt.Fprintln(stderr, hostUsage)
		return 2
	}
	rest, tokenFile := extractFlag(rest, "--token-file")
	hc, err := loadHostCtx(sel)
	if err != nil {
		fmt.Fprintln(stderr, "host:", err)
		return 1
	}
	if tokenFile != "" {
		if abs, err := filepath.Abs(tokenFile); err == nil {
			tokenFile = abs
		}
		hc.cfg.GitHubTokenFile = tokenFile
	}
	return hostRun(hc, stdout, stderr, rest)
}

// extractFlag pulls "--name v" or "--name=v" out of args.
func extractFlag(args []string, name string) (rest []string, val string) {
	for i := 0; i < len(args); i++ {
		n, v, has := strings.Cut(args[i], "=")
		if n != name {
			rest = append(rest, args[i])
			continue
		}
		if has {
			val = v
		} else if i+1 < len(args) {
			i++
			val = args[i]
		}
	}
	return rest, val
}

// hostRun dispatches a host subcommand on an already-loaded context.
func hostRun(hc *hostCtx, stdout, stderr io.Writer, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, hostUsage)
		return 2
	}
	switch args[0] {
	case "list":
		return hostList(hc, stdout, stderr, args[1:])
	case "check-key":
		return hostCheckKey(hc, stdout, stderr, args[1:])
	case "retire":
		return hostRetire(hc, stdout, stderr, args[1:])
	}
	fmt.Fprintf(stderr, "host: unknown action %q\n%s\n", args[0], hostUsage)
	return 2
}

type jsonKey struct {
	Repo      string `json:"repo"`
	ReadOnly  bool   `json:"readOnly"`
	CreatedAt string `json:"createdAt,omitempty"`
	LastUsed  string `json:"lastUsed,omitempty"`
}

type jsonHost struct {
	Label      string          `json:"label"`
	Keys       []jsonKey       `json:"keys"`
	LastUsed   string          `json:"lastUsed,omitempty"`
	Recipient  map[string]bool `json:"recipient"`
	InRegistry bool            `json:"inRegistry"`
	Expiry     string          `json:"expiry,omitempty"`
	CheckKey   *hosts.CheckKey `json:"checkKey,omitempty"`
	Stale      bool            `json:"stale"`
	Expired    bool            `json:"expired"`
}

func tsOrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func hostList(hc *hostCtx, stdout, stderr io.Writer, args []string) int {
	fs := flag.NewFlagSet("host list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	org := fs.String("org", "", "GitHub org (default: config githubOrg, else the owner of the remote)")
	stale := fs.Int("stale-days", hosts.DefaultStaleDays, "flag a host whose keys were not used for this many days")
	asJSON := fs.Bool("json", false, "machine-readable output")
	warn := fs.Int("check-key-warn", 0, "warn at this many live check-App keys (default: config checkKeyWarn, else 20)")
	if fs.Parse(args) != nil {
		return 2
	}
	if *stale <= 0 {
		fmt.Fprintln(stderr, "host list: --stale-days must be a positive number of days")
		return 2
	}
	if *warn <= 0 {
		*warn = hc.cfg.CheckKeyWarn
	}
	o, err := hc.orgName(*org)
	if err != nil {
		fmt.Fprintln(stderr, "host list:", err)
		return 2
	}
	c, err := hc.client()
	if err != nil {
		fmt.Fprintln(stderr, "host list:", err)
		return 1
	}
	rep, err := hc.buildReport(c, o, *stale, *warn)
	if err != nil {
		fmt.Fprintln(stderr, "host list:", err)
		return 1
	}
	if *asJSON {
		out := struct {
			Hosts    []jsonHost `json:"hosts"`
			Warnings []string   `json:"warnings,omitempty"`
		}{Hosts: []jsonHost{}, Warnings: rep.Warnings()}
		for _, h := range rep.Hosts {
			jh := jsonHost{Label: h.Label, Keys: []jsonKey{}, LastUsed: tsOrEmpty(h.LastUsed), Recipient: h.Recipient,
				InRegistry: h.InRegistry, Expiry: dateOrEmpty(h.Expiry), CheckKey: h.CheckKey, Stale: h.Stale, Expired: h.Expired}
			for _, k := range h.Keys {
				jh.Keys = append(jh.Keys, jsonKey{Repo: k.Repo, ReadOnly: k.ReadOnly, CreatedAt: tsOrEmpty(k.CreatedAt), LastUsed: tsOrEmpty(k.LastUsed)})
			}
			out.Hosts = append(out.Hosts, jh)
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	hdr := "LABEL\tLAST USED\tFLAGS\tDEPLOY KEYS"
	if len(rep.LocalRepos) > 0 {
		hdr += "\tRECIPIENT IN"
	}
	if rep.RegistryKnown {
		hdr += "\tREGISTRY\tEXPIRY\tCHECK KEY"
	}
	fmt.Fprintln(tw, hdr)
	for _, h := range rep.Hosts {
		var flags []string
		if h.Stale {
			flags = append(flags, "stale")
		}
		if h.Expired {
			flags = append(flags, "expired")
		}
		var ks []string
		for _, k := range h.Keys {
			mode := "rw"
			if k.ReadOnly {
				mode = "ro"
			}
			ks = append(ks, k.Repo+":"+mode)
		}
		row := fmt.Sprintf("%s\t%s\t%s\t%s", h.Label, dash(dateOrEmpty(h.LastUsed)), dash(strings.Join(flags, ",")), dash(strings.Join(ks, " ")))
		if len(rep.LocalRepos) > 0 {
			var in []string
			for _, r := range rep.LocalRepos {
				if h.Recipient[r] {
					in = append(in, r)
				}
			}
			row += "\t" + dash(strings.Join(in, " "))
		}
		if rep.RegistryKnown {
			reg := "no"
			if h.InRegistry {
				reg = "yes"
			}
			ck := "-"
			if h.CheckKey != nil {
				ck = h.CheckKey.Fingerprint + " (" + h.CheckKey.App + ")"
			}
			row += fmt.Sprintf("\t%s\t%s\t%s", reg, dash(dateOrEmpty(h.Expiry)), ck)
		}
		fmt.Fprintln(tw, row)
	}
	_ = tw.Flush()
	if len(rep.Hosts) == 0 {
		fmt.Fprintln(stdout, "no hosts found: no deploy keys on the mem-* repos")
	}
	for _, a := range rep.CheckKeys {
		fmt.Fprintf(stdout, "check keys: %s %d live\n", a.App, a.Live)
	}
	for _, w := range rep.Warnings() {
		fmt.Fprintln(stderr, "warning:", w)
	}
	return 0
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func dateOrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02")
}

func (hc *hostCtx) ownLabel() (string, error) {
	switch len(hc.ownLabels) {
	case 1:
		return hc.ownLabels[0], nil
	case 0:
		return "", errors.New("this host has no hostLabel: set one with `init --host-label` or in config.json")
	}
	return "", fmt.Errorf("this host's repos name several labels (%s); fix hostLabel in config.json", strings.Join(hc.ownLabels, ", "))
}

func hostCheckKey(hc *hostCtx, stdout, stderr io.Writer, args []string) int {
	fs := flag.NewFlagSet("host check-key", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fp := fs.String("fingerprint", "", "the key's fingerprint as GitHub shows it (SHA256:...); never the key")
	app := fs.String("app", "", "the check App's slug")
	clear := fs.Bool("clear", false, "remove this host's record")
	if fs.Parse(args) != nil {
		return 2
	}
	label, err := hc.ownLabel()
	if err != nil {
		fmt.Fprintln(stderr, "host check-key:", err)
		return 1
	}
	if hc.registryDir == "" {
		fmt.Fprintln(stderr, "host check-key: no registry configured (routes.json \"registry\")")
		return 1
	}
	if hc.registryReadOnly {
		fmt.Fprintln(stderr, "host check-key: this host is a read-only member of the registry and never commits; record it from a writable host")
		return 1
	}
	if *clear {
		if err := hosts.ClearCheckKey(hc.registryDir, label); err != nil {
			fmt.Fprintln(stderr, "host check-key:", err)
			return 1
		}
		fmt.Fprintf(stdout, "cleared the check key record of %s (committed locally in the registry)\n", label)
		return 0
	}
	if *fp == "" || *app == "" {
		fmt.Fprintln(stderr, "host check-key: --fingerprint and --app are required (or --clear)")
		return 2
	}
	if _, err := hosts.WriteCheckKey(hc.registryDir, label, hosts.CheckKey{Fingerprint: *fp, App: *app}); err != nil {
		fmt.Fprintln(stderr, "host check-key:", err)
		return 1
	}
	fmt.Fprintf(stdout, "recorded the check key of %s in the registry (committed locally; the registry's sync pushes it)\n", label)
	return 0
}

// publishExpiry writes this host's expiresAt into the registry as
// hosts/<label>/expiry (committed locally; the registry's own sync pushes it)
// so `host list` on other hosts can flag it. It does nothing without an expiry,
// and never commits into a registry this host is a read-only member of.
func publishExpiry(cfg config.Config, registryDir string, registryReadOnly bool, logf func(string, ...any)) error {
	d, ok := cfg.ExpiryDate()
	if !ok || registryDir == "" {
		return nil
	}
	if cfg.HostLabel == "" {
		return errors.New("expiresAt is set but hostLabel is not: the expiry cannot be published under a name")
	}
	if registryReadOnly {
		logf("expiry not published: this host is a read-only member of the registry")
		return nil
	}
	changed, err := hosts.WriteExpiry(registryDir, cfg.HostLabel, d)
	if err != nil {
		return err
	}
	if changed {
		logf("published the expiry %s as hosts/%s/expiry in the registry (committed locally)", cfg.ExpiresAt, cfg.HostLabel)
	}
	return nil
}

// expiryLine is the status line for a host with an expiry date.
func expiryLine(cfg config.Config, now time.Time) string {
	if cfg.ExpiresAt == "" {
		return ""
	}
	if cfg.Expired(now) {
		return "expired: retire me (expiresAt " + cfg.ExpiresAt + "): syncing is held"
	}
	return "expires on " + cfg.ExpiresAt
}
