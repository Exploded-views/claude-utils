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
	p := defaultRoutesPath()
	if _, err := os.Stat(p); err == nil {
		return p, true, nil
	}
	return "", false, nil
}

// routedSet is a loaded routes.json: the router, plus the config of every repo
// this host has.
type routedSet struct {
	Path    string
	File    route.File
	Router  *route.Router
	Cfgs    map[string]config.Config // repos this host has
	Missing []string                 // repos named in routes.json that this host lacks
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
			if err := loop.Run(ctx); err != nil {
				log.Printf("[%s] stopped: %v", loop.Name, err)
				r.fail(fmt.Errorf("repo %q: %w", loop.Name, err))
			}
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
				s.Router.Refresh()
			}
		}
	}()
	return r, nil
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
	for _, name := range s.File.RepoNames() {
		cfg, have := s.Cfgs[name]
		switch {
		case !have:
			lines = append(lines, fmt.Sprintf("  %s: not on this host (folders routed to it are denied)", name))
		case name == s.File.Personal:
			lines = append(lines, fmt.Sprintf("  %s: personal, owns no folders; syncs %s", name, cfg.SyncDir))
		default:
			lines = append(lines, fmt.Sprintf("  %s: owns %d %s; syncs %s", name, sum.Owned[name], plural(sum.Owned[name], "folder", "folders"), cfg.SyncDir))
		}
		if have {
			if l := pausedLine(cfg); l != "" {
				lines = append(lines, "    "+strings.ReplaceAll(l, "\n", " "))
			}
		}
	}
	lines = append(lines, fmt.Sprintf("  denied: %d %s", sum.Denied, plural(sum.Denied, "folder", "folders")))
	for _, sh := range s.Router.Shadowed() {
		lines = append(lines, fmt.Sprintf("  shadowed: rule %d is never reached first (rule %d matches the same folders)", sh.Rule, sh.By))
	}
	return lines
}
