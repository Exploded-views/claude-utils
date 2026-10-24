package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/lock"
	"github.com/MarimerLLC/claude-utils/internal/route"
	syncpkg "github.com/MarimerLLC/claude-utils/internal/sync"
)

// routesEnv overrides the routes.json location (tests, unusual layouts).
const routesEnv = "CLAUDE_MEMSYNC_ROUTES"

// defaultRoutesPath is ~/.config/claude-memsync/routes.json, per host and never
// synced.
func defaultRoutesPath() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "claude-memsync", "routes.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "claude-memsync", "routes.json")
}

// knownRoutesPaths lists the places a routes file may be without being named:
// the environment variable's path, then $XDG_CONFIG_HOME's and ~/.config's.
func knownRoutesPaths() []string {
	var out []string
	add := func(p string) {
		if p == "" {
			return
		}
		for _, o := range out {
			if o == p {
				return
			}
		}
		out = append(out, p)
	}
	add(os.Getenv(routesEnv))
	add(defaultRoutesPath())
	home, _ := os.UserHomeDir()
	add(filepath.Join(home, ".config", "claude-memsync", "routes.json"))
	return out
}

// legacyGuard refuses single-repo mode on a host that has a routes file at any
// known place: such a host's folders belong to several repos, and one daemon
// mirroring all of them into one repo would undo the routing.
func legacyGuard(cfg config.Config) error {
	for _, p := range knownRoutesPaths() {
		if _, err := os.Stat(p); err == nil {
			return fmt.Errorf("a routes file exists at %s: run through it (--routes %s) instead of syncing %s on its own", p, p, cfg.SyncDir)
		}
	}
	return nil
}

// resolveRoutesPath picks the routes file: the flag, then the environment, then
// the default location if a file is there. ok is false for single-repo mode.
// A flag or environment path that does not exist is an error, not a fallback:
// a typo must not silently mirror everything into one repo.
func resolveRoutesPath(flagVal string) (path string, ok bool, err error) {
	explicit := flagVal
	if explicit == "" {
		explicit = os.Getenv(routesEnv)
	}
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", false, fmt.Errorf("routes file: %w", err)
		}
		return explicit, true, nil
	}
	for _, p := range knownRoutesPaths() {
		if _, err := os.Stat(p); err == nil {
			return p, true, nil
		}
	}
	return "", false, nil
}

// routedSet is a loaded routes.json: the router, plus the config of every repo
// this host has.
type routedSet struct {
	reloadErr string // last reload failure, logged once
	Path      string
	File      route.File
	Router    *route.Router
	Cfgs      map[string]config.Config // repos this host has
	Missing   []string                 // repos named in routes.json that this host lacks
}

func loadRouted(path string) (*routedSet, error) {
	f, err := route.Load(path)
	if err != nil {
		return nil, err
	}
	s := &routedSet{Path: path, File: f, Cfgs: map[string]config.Config{}}
	for _, name := range f.RepoNames() {
		cp := route.ExpandHome(f.Repos[name].Config)
		cfg, err := config.Load(cp)
		switch {
		case err == nil:
			if err := checkKindAgainstRemote(name, f.Repos[name].Kind, cfg.RemoteURL); err != nil {
				return nil, err
			}
			s.Cfgs[name] = cfg
		case errors.Is(err, fs.ErrNotExist):
			s.Missing = append(s.Missing, name)
		default:
			return nil, fmt.Errorf("repo %q: %w", name, err)
		}
	}
	seen := map[string]string{}
	for _, name := range f.RepoNames() {
		if cfg, ok := s.Cfgs[name]; ok {
			if other, dup := seen[cfg.SyncDir]; dup {
				return nil, fmt.Errorf("repos %q and %q share the sync dir %s", other, name, cfg.SyncDir)
			}
			seen[cfg.SyncDir] = name
		}
	}
	s.Router = route.New(f, func(repo string) bool { _, ok := s.Cfgs[repo]; return ok })
	s.Router.StatePath = filepath.Join(filepath.Dir(path), "owners.json")
	return s, nil
}

// checkKindAgainstRemote refuses a repo whose remote is named like a client or
// orchestrator repo (mem-client-*, mem-orch-*) but whose kind says otherwise:
// that mistake would let a client's notes into the personal catalog.
func checkKindAgainstRemote(name, kind, remote string) error {
	base := strings.TrimSuffix(strings.TrimRight(remote, "/"), ".git")
	if i := strings.LastIndexAny(base, "/:\\"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.ToLower(base)
	for prefix, want := range map[string]string{"mem-client-": route.KindClient, "mem-orch-": route.KindOrch} {
		if strings.HasPrefix(base, prefix) && kind != want {
			return fmt.Errorf("repo %q: its remote is named %s... so its kind must be %q, not %q", name, prefix, want, kind)
		}
	}
	return nil
}

// repoForSyncDir names the repo of routes.json whose config.json lives in dir.
func (s *routedSet) repoForSyncDir(dir string) string {
	want, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for _, name := range s.File.RepoNames() {
		got, err := filepath.Abs(filepath.Dir(route.ExpandHome(s.File.Repos[name].Config)))
		if err == nil && filepath.Clean(got) == filepath.Clean(want) {
			return name
		}
	}
	return ""
}

// Primary is the config the daemon's PID file and the default commands use:
// the personal repo if this host has it, else the first repo by name.
func (s *routedSet) Primary() (config.Config, bool) {
	if c, ok := s.Cfgs[s.File.Personal]; ok && s.File.Personal != "" {
		return c, true
	}
	for _, n := range s.File.RepoNames() {
		if c, ok := s.Cfgs[n]; ok {
			return c, true
		}
	}
	return config.Config{}, false
}

// names lists the repos this host has, sorted.
func (s *routedSet) names() []string {
	var out []string
	for n := range s.Cfgs {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// running is a started daemon body: Stop cancels it and waits.
type running struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
	locks  []*lock.Lock
	mu     sync.Mutex
	err    error
}

func (r *running) fail(err error) {
	r.mu.Lock()
	r.err = errors.Join(r.err, err)
	r.mu.Unlock()
}

// Stop ends every loop, waits for them and drops the locks.
func (r *running) Stop() error {
	r.cancel()
	r.wg.Wait()
	for _, l := range r.locks {
		l.Release()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// Wait blocks until every loop has ended.
func (r *running) Wait() { r.wg.Wait() }

// startSingle runs one Loop on cfg, as before routing existed, but refuses a
// second daemon on the same sync dir.
func startSingle(ctx context.Context, cfg config.Config) (*running, error) {
	if err := legacyGuard(cfg); err != nil {
		return nil, err
	}
	l, err := lock.Acquire(cfg.SyncDir)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	r := &running{cancel: cancel, locks: []*lock.Lock{l}}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		loop := &syncpkg.Loop{Cfg: cfg}
		if err := loop.Run(ctx); err != nil {
			r.fail(err)
		}
	}()
	return r, nil
}

// Start takes the lock of every sync dir this host has (all or nothing), then
// runs one Loop per repo, each gated by the router. One loop failing does not
// stop the others.
func (s *routedSet) Start(ctx context.Context) (*running, error) {
	if len(s.Cfgs) == 0 {
		return nil, fmt.Errorf("%s: this host has none of the repos", s.Path)
	}
	ctx, cancel := context.WithCancel(ctx)
	r := &running{cancel: cancel}
	for _, name := range s.names() {
		l, err := lock.Acquire(s.Cfgs[name].SyncDir)
		if err != nil {
			for _, h := range r.locks {
				h.Release()
			}
			cancel()
			return nil, fmt.Errorf("repo %q: %w", name, err)
		}
		r.locks = append(r.locks, l)
	}
	for _, m := range s.Missing {
		log.Printf("routing: repo %q is not on this host; folders routed to it are denied", m)
	}
	interval := time.Hour
	for _, name := range s.names() {
		cfg := s.Cfgs[name]
		if d := time.Duration(cfg.PullIntervalSec) * time.Second; d > 0 && d < interval {
			interval = d
		}
		loop := &syncpkg.Loop{Cfg: cfg, Name: name, Owns: s.Router.Owns(name)}
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			superviseLoop(ctx, loop)
		}()
	}
	// Checkouts and remotes change; decisions are re-read on the pull tick.
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.reload()
				s.Router.Refresh()
			}
		}
	}()
	return r, nil
}

// reload re-reads routes.json and swaps the table in atomically. A file that no
// longer parses leaves the old table in force and is logged once.
func (s *routedSet) reload() {
	f, err := route.Load(s.Path)
	if err != nil {
		if msg := err.Error(); msg != s.reloadErr {
			s.reloadErr = msg
			log.Printf("routing: keeping the previous routes, %v", err)
		}
		return
	}
	s.reloadErr = ""
	s.Router.Replace(f)
}

// Loop supervision. A loop that dies is recorded in .state/loop-failed (which
// `status` shows) and restarted with a growing delay.
var (
	loopBackoffMin = 5 * time.Second
	loopBackoffMax = 5 * time.Minute
	loopHealthy    = 30 * time.Second
	runLoop        = func(ctx context.Context, l *syncpkg.Loop) error { return l.Run(ctx) }
)

func loopFailedPath(syncDir string) string { return filepath.Join(syncDir, ".state", "loop-failed") }

func superviseLoop(ctx context.Context, loop *syncpkg.Loop) {
	backoff := loopBackoffMin
	failed := loopFailedPath(loop.Cfg.SyncDir)
	for {
		healthy := time.AfterFunc(loopHealthy, func() { os.Remove(failed) })
		started := time.Now()
		err := runLoop(ctx, loop)
		healthy.Stop()
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("loop returned")
		}
		msg := strings.SplitN(err.Error(), "\n", 2)[0]
		log.Printf("[%s] stopped: %s; restarting in %s", loop.Name, msg, backoff)
		_ = os.MkdirAll(filepath.Dir(failed), 0700)
		_ = os.WriteFile(failed, []byte(time.Now().UTC().Format(time.RFC3339)+" "+msg+"\n"), 0600)
		if time.Since(started) > loopHealthy {
			backoff = loopBackoffMin
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > loopBackoffMax {
			backoff = loopBackoffMax
		}
	}
}

// claudeFolders lists the project folders in the Claude tree.
func claudeFolders(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

// statusLines describes the routing: each repo, how many folders it owns, how
// many are denied, and the rules an earlier rule shadows.
func (s *routedSet) statusLines() []string {
	lines := []string{"routing: " + s.Path}
	primary, _ := s.Primary()
	sum := s.Router.Summarize(claudeFolders(primary.ClaudeProjectsDir))
	for _, name := range s.Router.File().RepoNames() {
		cfg, have := s.Cfgs[name]
		switch {
		case !have:
			lines = append(lines, fmt.Sprintf("  %s: not on this host (folders routed to it are denied)", name))
		case name == s.Router.File().Personal:
			lines = append(lines, fmt.Sprintf("  %s: personal, owns no folders; syncs %s", name, cfg.SyncDir))
		default:
			lines = append(lines, fmt.Sprintf("  %s: owns %d %s; syncs %s", name, sum.Owned[name], plural(sum.Owned[name], "folder", "folders"), cfg.SyncDir))
		}
		if have {
			if b, err := os.ReadFile(loopFailedPath(cfg.SyncDir)); err == nil {
				lines = append(lines, "    loop stopped, restarting with backoff: "+strings.TrimSpace(string(b)))
			}
			if l := pausedLine(cfg); l != "" {
				lines = append(lines, "    "+strings.ReplaceAll(l, "\n", " "))
			}
		}
	}
	lines = append(lines, fmt.Sprintf("  denied: %d %s", sum.Denied, plural(sum.Denied, "folder", "folders")))
	for _, rf := range sum.Renamed {
		lines = append(lines, fmt.Sprintf("  renamed remote: add an alias (folder %s is routed by its root commits for %d more %s)", rf.Hash, rf.DaysLeft, plural(rf.DaysLeft, "day", "days")))
	}
	for _, h := range sum.Expired {
		lines = append(lines, fmt.Sprintf("  renamed remote: folder %s is denied, the grace period is over; add an alias", h))
	}
	for _, h := range sum.Changed {
		lines = append(lines, fmt.Sprintf("  remote changed repos: folder %s is denied until routes.json is edited", h))
	}
	for _, sh := range s.Router.Shadowed() {
		lines = append(lines, fmt.Sprintf("  shadowed: rule %d is never reached first (rule %d matches the same folders)", sh.Rule, sh.By))
	}
	return lines
}
