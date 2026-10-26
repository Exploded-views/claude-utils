package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/ghguard"
	"github.com/MarimerLLC/claude-utils/internal/related"
)

// errUnverified marks a repo whose visibility could not be established (the
// API was unreachable or answered with an error) and has no usable cached
// verdict: the edge fails closed.
var errUnverified = errors.New("unverified")

var (
	// visTTL is how long a verdict stays fresh.
	visTTL = time.Hour
	// quickTimeout bounds an API call made from a hook or the session context.
	quickTimeout = 2 * time.Second
	// visMaxStale is how old a cached verdict may be and still serve when the
	// API cannot be reached; older ones fail closed.
	visMaxStale = 24 * time.Hour
)

type verdict struct {
	Private bool      `json:"private"`
	At      time.Time `json:"at"`
}

// ghVisibility is the visibility check of a relation's endpoints. The name must
// be a mem-* name; where a GitHub token is configured (the primary repo's
// config, as for doctor) the repo must also read back as private. A token that
// is configured but unusable fails closed; with no token the check is names
// only, as the private-only mode itself is without one.
//
// Verdicts are cached per repo in the related state dir for visTTL. In quick
// mode (hooks, session context) the API gets quickTimeout and an expired
// verdict is still used when the API cannot be reached; a repo with no verdict
// at all is "unverified".
//
// The API client is built lazily, only when a verdict is really needed: building
// it can run the token command or mint an app token over the network. In
// offline mode (the hook paths) it is never built: only cached verdicts count,
// and a stale or missing one hides the related repo for this run.
type ghVisibility struct {
	names     related.NameOnly
	cfg       config.Config
	hasCfg    bool // a primary repo's config is in cfg
	client    *ghguard.Client
	loadErr   error
	built     bool
	build     func() (*ghguard.Client, error)
	offline   bool
	warn      func(string) // offline: told once that a verdict is missing or stale
	warned    bool
	remote    func(repo string) string // the configured remote URL of a repo this host has, or ""
	cachePath string
	quick     bool
	maxStale  time.Duration // 0: visMaxStale
	now       func() time.Time
	mu        sync.Mutex
}

func newGHVisibility(set *routedSet) *ghVisibility {
	v := &ghVisibility{now: time.Now, cfg: config.Defaults()}
	cfg, ok := set.Primary()
	if !ok {
		return v
	}
	v.cfg, v.hasCfg = cfg, true
	v.build = func() (*ghguard.Client, error) { return guardClient(cfg) }
	v.remote = func(repo string) string {
		if k, ok := set.localKey(repo); ok {
			return set.Cfgs[k].RemoteURL
		}
		return ""
	}
	return v
}

// getClient builds the API client on first use. A nil client with a nil error
// means no usable token: the check is names only.
func (v *ghVisibility) getClient() (*ghguard.Client, error) {
	if !v.built {
		v.built = true
		if v.build != nil {
			v.client, v.loadErr = v.build()
		}
	}
	return v.client, v.loadErr
}

// tokenConfigured says whether the primary repo's config names a token source,
// without running or reading any of them but the environment.
func (v *ghVisibility) tokenConfigured() bool {
	if !v.hasCfg {
		return false
	}
	c := v.cfg
	if c.GitHubApp != "" || c.GitHubTokenFile != "" || len(c.GitHubTokenCmd) > 0 {
		return true
	}
	tok, err := ghguard.EnvSource{Name: c.GitHubTokenEnv}.Token(context.Background())
	return err != nil || tok != ""
}

func (v *ghVisibility) load() map[string]verdict {
	m := map[string]verdict{}
	if v.cachePath != "" {
		if b, err := os.ReadFile(v.cachePath); err == nil {
			_ = json.Unmarshal(b, &m)
		}
	}
	return m
}

func (v *ghVisibility) save(m map[string]verdict) {
	if v.cachePath == "" {
		return
	}
	if os.MkdirAll(filepath.Dir(v.cachePath), 0o700) != nil {
		return
	}
	b, _ := json.Marshal(m)
	tmp := v.cachePath + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, v.cachePath)
	}
}

// api returns a client for this call: the quick one has a short timeout.
func (v *ghVisibility) api(client *ghguard.Client) *ghguard.Client {
	if !v.quick {
		return client
	}
	c := *client
	c.HTTP = &http.Client{Timeout: quickTimeout}
	return &c
}

func privateErr(repo string, private bool) error {
	if private {
		return nil
	}
	return fmt.Errorf("%s is not private", repo)
}

// verify decides repo (owner/name on the API) from the cache or the API.
// The cache is keyed by the repo's full name, so the same name under another
// owner never inherits a verdict.
func (v *ghVisibility) verify(repo, owner, name string) error {
	key := strings.ToLower(owner + "/" + name)
	v.mu.Lock()
	defer v.mu.Unlock()
	m := v.load()
	cached, have := m[key]
	if have && v.now().Sub(cached.At) < visTTL {
		return privateErr(repo, cached.Private)
	}
	limit := v.maxStale
	if limit <= 0 {
		limit = visMaxStale
	}
	// A recent verdict that says "not private" counts whatever the token setup
	// and whether or not the API can be asked: the repo is hidden.
	if have && !cached.Private && v.now().Sub(cached.At) < limit {
		return privateErr(repo, false)
	}
	if !v.tokenConfigured() {
		return nil // no token: names only
	}
	if v.offline {
		if have && v.now().Sub(cached.At) < limit {
			return privateErr(repo, cached.Private)
		}
		if v.warn != nil && !v.warned {
			v.warned = true
			v.warn(fmt.Sprintf("%s: no recent visibility verdict cached; not shown this time (a command run by hand refreshes it)", repo))
		}
		return fmt.Errorf("%w: no recent cached verdict that %s is private", errUnverified, repo)
	}
	client, cerr := v.getClient()
	if cerr != nil {
		return fmt.Errorf("cannot check visibility: %w", cerr)
	}
	if client == nil {
		return nil // no usable token: names only
	}
	r, err := v.api(client).GetRepo(owner, name)
	if err != nil {
		if have && v.quick && v.now().Sub(cached.At) < limit {
			return privateErr(repo, cached.Private)
		}
		return fmt.Errorf("%w: cannot check that %s is private: %v", errUnverified, repo, err)
	}
	private := r.Private != nil && *r.Private && r.Visibility == "private"
	m[key] = verdict{Private: private, At: v.now()}
	v.save(m)
	return privateErr(repo, private)
}

func (v *ghVisibility) CheckPrivate(repo string) error {
	if err := v.names.CheckPrivate(repo); err != nil {
		return err
	}
	// Without a token source the check is names only, except that a recent
	// cached "not private" verdict still hides the repo (see verify). Problems
	// resolving the repo's owner matter only when a token makes the check real.
	tok := v.tokenConfigured()
	if v.remote != nil {
		if url := v.remote(repo); url != "" {
			owner, name, err := ghguard.ValidateRemote(url, allowedHost(v.cfg), hostResolver)
			if err != nil {
				if !tok {
					return nil
				}
				return fmt.Errorf("remote refused: %w", err)
			}
			return v.verify(repo, owner, name)
		}
	}
	org, err := orgFor(v.cfg, "")
	if err != nil {
		if !tok {
			return nil
		}
		return fmt.Errorf("cannot check visibility: %w", err)
	}
	return v.verify(repo, org, repo)
}

// skipCloneURLCheck lets tests fetch from a local bare repository.
var skipCloneURLCheck bool

// CheckCloneURL vets the exact URL a read clone is fetched from: a mem-* name
// on the allowed git host (the private-only offline rules), under this host's
// own org or an owner listed in owners, and, with a token, private.
func (v *ghVisibility) CheckCloneURL(repo, url string, owners []string) error {
	if skipCloneURLCheck {
		return nil
	}
	cfg := v.cfg
	cfg.RemoteURL = url
	cfg.SyncDir = ""
	if err := validateRemoteOffline(cfg); err != nil {
		return err
	}
	owner, name, err := ghguard.ValidateRemote(url, allowedHost(cfg), hostResolver)
	if err != nil {
		return fmt.Errorf("remote refused: %w", err)
	}
	if strings.ToLower(name) != repo {
		return fmt.Errorf("the URL names %q, not %s", name, repo)
	}
	allowed := append([]string(nil), owners...)
	if org, err := orgFor(v.cfg, ""); err == nil {
		allowed = append(allowed, org)
	}
	ok := false
	for _, a := range allowed {
		if strings.EqualFold(a, owner) {
			ok = true
		}
	}
	if !ok {
		return fmt.Errorf("the URL for %s is owned by %q, which is not this host's org; list it in relatedOwners to allow it", repo, owner)
	}
	return v.verify(repo, owner, name)
}
