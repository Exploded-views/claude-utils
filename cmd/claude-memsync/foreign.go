package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/foreign"
	"github.com/MarimerLLC/claude-utils/internal/route"
)

// foreignEnv is what the foreign-writer checks look at; tests replace it.
var foreignEnv = func() foreign.Env { return foreign.DefaultEnv(log.Printf) }

// quietWait sits through the start-up quiet window; tests replace it.
var quietWait = time.Sleep

// guardOpts are the flags of run and init that bear on the pre-start guard.
type guardOpts struct {
	IgnoreForeign bool // --ignore-foreign-writers
	ForceQuiet    bool // --quiet-window: run the window although this is not the first start
	SkipQuiet     bool // --skip-quiet-window: a deliberate first start without it
}

func (o guardOpts) validate() error {
	if o.ForceQuiet && o.SkipQuiet {
		return errors.New("--quiet-window and --skip-quiet-window contradict each other")
	}
	return nil
}

// quietMarker says the start-up quiet window has been dealt with for a repo:
// the migration moment is the first start after init. init removes it.
func quietMarker(syncDir string) string {
	return filepath.Join(syncDir, ".state", "quiet-window-done")
}

func markQuiet(syncDir string) {
	p := quietMarker(syncDir)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err == nil {
		_ = os.WriteFile(p, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
	}
}

func hasQuietMarker(syncDir string) bool {
	_, err := os.Stat(quietMarker(syncDir))
	return err == nil
}

// checkForeign is the list part of the guard. It can be overridden; an entry
// that could not be checked on this host refuses like an active one.
func checkForeign(writers []foreign.Writer, ignore bool) error {
	r := foreign.Inspect(writers, foreignEnv())
	if err := foreign.Refusal(r); err != nil {
		if !ignore {
			return err
		}
		for _, a := range r.Active {
			log.Printf("warning: %s is active; starting anyway (--ignore-foreign-writers)", a)
		}
		for _, a := range r.Unchecked {
			log.Printf("warning: %s could not be checked; starting anyway (--ignore-foreign-writers)", a)
		}
	}
	return nil
}

// startGuard is the guard for `run`, in routed or single-repo mode. A config
// that cannot be read is left for the start itself to report.
//
// The foreignWriters list is the primary guard. The quiet window is a weak
// extra signal (a busy host writes memory all the time), so it only runs on the
// first start after init of each repo, or when forced.
func startGuard(configPath, routes string, routed bool, opts guardOpts) error {
	if err := opts.validate(); err != nil {
		return err
	}
	type repo struct {
		syncDir string
		target  foreign.Target
		quiet   int
	}
	var repos []repo
	var writers []foreign.Writer
	if routed {
		set, err := loadRouted(routes)
		if err != nil {
			return nil
		}
		writers = append(writers, set.File.ForeignWriters...)
		for _, name := range set.names() {
			cfg := set.Cfgs[name]
			writers = append(writers, cfg.ForeignWriters...)
			repos = append(repos, repo{cfg.SyncDir, foreign.Target{Claude: cfg.ClaudeProjectsDir, Owns: set.Router.Owns(name)}, cfg.QuietSec()})
		}
	} else {
		cfg, err := config.Load(configPath)
		if err != nil {
			return nil
		}
		writers = cfg.ForeignWriters
		repos = []repo{{cfg.SyncDir, foreign.Target{Claude: cfg.ClaudeProjectsDir}, cfg.QuietSec()}}
	}
	if err := checkForeign(writers, opts.IgnoreForeign); err != nil {
		return err
	}
	var targets []foreign.Target
	var first []string
	quiet := 0
	for _, r := range repos {
		if hasQuietMarker(r.syncDir) && !opts.ForceQuiet {
			continue
		}
		first = append(first, r.syncDir)
		targets = append(targets, r.target)
		if r.quiet > quiet {
			quiet = r.quiet
		}
	}
	if opts.SkipQuiet {
		log.Printf("warning: skipping the start-up quiet window (--skip-quiet-window)")
	} else if err := foreign.QuietCheck(targets, quiet, quietWait); err != nil {
		return err
	}
	for _, d := range first {
		markQuiet(d)
	}
	return nil
}

// initGuard is the same guard for init, which is the migration moment: the
// quiet window always applies unless skipped. The list comes from the routes
// file if the host has one (one that will not load refuses), else from the sync
// dir's config.json when it already exists (a re-init).
func initGuard(cfg config.Config, owns func(string) bool, opts guardOpts) error {
	if err := opts.validate(); err != nil {
		return err
	}
	var writers []foreign.Writer
	quiet := cfg.QuietSec()
	path, ok, err := resolveRoutesPath("")
	if err != nil {
		return err
	}
	if ok {
		f, err := route.Load(path)
		if err != nil {
			return fmt.Errorf("foreign-writer list: %w", err)
		}
		writers = f.ForeignWriters
	}
	if old, err := config.Load(cfg.Path()); err == nil {
		writers = append(append([]foreign.Writer{}, writers...), old.ForeignWriters...)
		if !ok {
			quiet = old.QuietSec()
		}
	}
	if err := checkForeign(writers, opts.IgnoreForeign); err != nil {
		return err
	}
	if opts.SkipQuiet {
		log.Printf("warning: skipping the start-up quiet window (--skip-quiet-window)")
		return nil
	}
	return foreign.QuietCheck([]foreign.Target{{Claude: cfg.ClaudeProjectsDir, Owns: owns}}, quiet, quietWait)
}

// foreignStatusLines reports an active foreign writer for `status` in
// single-repo mode; the routed form is per repo (see routedSet.foreignLines).
func foreignStatusLines() []string {
	cfg, err := config.Load(defaultConfigPath())
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range foreign.Check(cfg.ForeignWriters, foreignEnv()) {
		out = append(out, "blocked: "+a.String())
	}
	return out
}
