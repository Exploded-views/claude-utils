package main

import (
	"fmt"
	"log"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/foreign"
	"github.com/MarimerLLC/claude-utils/internal/route"
)

// foreignEnv is what the foreign-writer checks look at; tests replace it.
var foreignEnv = func() foreign.Env { return foreign.DefaultEnv(log.Printf) }

// quietWait sits through the start-up quiet window; tests replace it.
var quietWait = time.Sleep

// preStart is the guard that runs before a run or an init mirrors anything.
// The foreign-writer list can be overridden; the quiet window cannot.
func preStart(writers []foreign.Writer, targets []foreign.Target, quietSec int, ignore bool) error {
	if act := foreign.Check(writers, foreignEnv()); len(act) > 0 {
		if !ignore {
			return foreign.Refusal(act)
		}
		for _, a := range act {
			log.Printf("warning: %s is active; starting anyway (--ignore-foreign-writers)", a)
		}
	}
	return foreign.QuietCheck(targets, quietSec, quietWait)
}

// startGuard runs preStart for `run`, in routed or single-repo mode. A config
// that cannot be read is left for the start itself to report.
func startGuard(configPath, routes string, routed, ignore bool) error {
	if routed {
		set, err := loadRouted(routes)
		if err != nil {
			return nil
		}
		quiet := 0
		var targets []foreign.Target
		for _, name := range set.names() {
			cfg := set.Cfgs[name]
			targets = append(targets, foreign.Target{Claude: cfg.ClaudeProjectsDir, Owns: set.Router.Owns(name)})
			if q := cfg.QuietSec(); q > quiet {
				quiet = q
			}
		}
		return preStart(set.File.ForeignWriters, targets, quiet, ignore)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil
	}
	return preStart(cfg.ForeignWriters, []foreign.Target{{Claude: cfg.ClaudeProjectsDir}}, cfg.QuietSec(), ignore)
}

// initGuard is the same guard for init. The list comes from the routes file if
// the host has one, else from the sync dir's config.json when it already
// exists (a re-init).
func initGuard(cfg config.Config, owns func(string) bool, ignore bool) error {
	var writers []foreign.Writer
	quiet := cfg.QuietSec()
	if path, ok, err := resolveRoutesPath(""); err == nil && ok {
		if f, err := route.Load(path); err == nil {
			writers = f.ForeignWriters
		}
	} else if old, err := config.Load(cfg.Path()); err == nil {
		writers, quiet = old.ForeignWriters, old.QuietSec()
	}
	return preStart(writers, []foreign.Target{{Claude: cfg.ClaudeProjectsDir, Owns: owns}}, quiet, ignore)
}

// foreignStatusLines reports an active foreign writer for `status`.
func foreignStatusLines() []string {
	var writers []foreign.Writer
	if path, ok, err := resolveRoutesPath(""); err == nil && ok {
		f, err := route.Load(path)
		if err != nil {
			return nil
		}
		writers = f.ForeignWriters
	} else if cfg, err := config.Load(defaultConfigPath()); err == nil {
		writers = cfg.ForeignWriters
	}
	var out []string
	for _, a := range foreign.Check(writers, foreignEnv()) {
		out = append(out, fmt.Sprintf("blocked: %s", a))
	}
	return out
}
