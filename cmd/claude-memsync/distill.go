package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/distill"
	"github.com/MarimerLLC/claude-utils/internal/lock"
)

// runDistill implements `claude-memsync distill`. It regenerates the
// DISTILLED.md index from the catalog entry files the /distill skill produced,
// optionally prunes stale entries, and reports the worklist of marked-but-not-
// yet-distilled memories. It performs no classification — that is the skill's
// job; this is the mechanical half.
func runDistill(args []string) int {
	flags := flag.NewFlagSet("distill", flag.ContinueOnError)
	cfgPath := flags.String("config", "", "path to config.json (default: ~/.claudesync/config.json)")
	repoName := flags.String("repo", "", "with a routes file: the repo to rebuild (a key of routes.json repos; default: the personal repo)")
	routesFlag := flags.String("routes", "", "path to routes.json (default: $"+routesEnv+", then ~/.config/claude-memsync/routes.json if it exists)")
	prune := flags.Bool("prune", false, "remove catalog entries whose source memory lost the marker or vanished (archived instead when archiveDeletes is on)")
	dryRun := flags.Bool("dry-run", false, "report what would change without writing the index or pruning")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	cfg, _, err := resolveRepo(repoSel{*repoName, *cfgPath, *routesFlag}, false)
	if err != nil {
		fmt.Fprintln(os.Stderr, "distill:", err)
		return 1
	}
	if !isPersonalRepoIn(*routesFlag, cfg) {
		fmt.Fprintln(os.Stderr, "distill: the catalog lives in the personal repo; this repo is not it")
		return 1
	}
	opts := distill.Options{
		ProjectsDir:  cfg.ClaudeProjectsDir,
		DistilledDir: cfg.DistilledPath(),
	}
	// With a routes file, only folders routed to non-client repos feed the
	// catalog, whatever --config says: a client's notes never enter the worklist.
	// A routes file that cannot be used stops distill rather than lifting the
	// filter.
	keep, err := distillKeep(*routesFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "distill:", err)
		return 1
	}
	opts.Keep = keep
	if cfg.ArchiveDeletes {
		opts.ArchiveRoot = cfg.SyncDir
	}

	var res distill.Result
	switch {
	case *dryRun:
		res, err = distill.Preview(opts)
	case *prune:
		// Pruning deletes or archives synced catalog files: it must not race
		// the daemon of this repo, so it takes that sync dir's lock. (The
		// index alone is a git-ignored derived file; rebuilding it needs none.)
		err = withSyncLockStopping(os.Stderr, cfg.SyncDir, func() (e error) { res, e = distill.Run(opts, true); return })
		if errors.Is(err, lock.ErrHeld) {
			fmt.Fprintln(os.Stderr, "distill: the daemon is running on this sync dir; run `claude-memsync stop`, then --prune, then `claude-memsync start`")
		}
	default:
		res, err = distill.Run(opts, false)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "distill:", err)
		return 1
	}

	report(res, *dryRun, cfg.ArchiveDeletes)
	return 0
}

// distillKeep is the worklist filter: nil without a routes file, an error when
// one exists and is unusable.
func distillKeep(routesFlag string) (func(string) bool, error) {
	path, ok, err := resolveRoutesPath(routesFlag)
	if err != nil || !ok {
		return nil, err
	}
	set, err := loadRouted(path)
	if err != nil {
		return nil, err
	}
	return set.Router.NonClient, nil
}

// loadDistillConfig loads config.json, falling back to platform defaults when
// no config exists yet (the catalog can be indexed before `init` has run).
func loadDistillConfig(cfgPath string) (config.Config, error) {
	if cfgPath == "" {
		// With several repos the catalog lives in the personal one.
		if path, ok, err := resolveRoutesPath(""); err != nil {
			return config.Config{}, err
		} else if ok {
			set, err := loadRouted(path)
			if err != nil {
				return config.Config{}, err
			}
			if c, ok := set.Primary(); ok {
				return c, nil
			}
		}
		cfgPath = defaultConfigPath()
	}
	cfg, err := loadConfig(cfgPath)
	if errors.Is(err, fs.ErrNotExist) {
		return config.Defaults(), nil
	}
	return cfg, err
}

func report(res distill.Result, dryRun, archived bool) {
	verb := "indexed"
	if dryRun {
		verb = "would index"
	}
	fmt.Printf("%s %d distilled %s\n", verb, res.Indexed, plural(res.Indexed, "entry", "entries"))
	if res.Pruned > 0 {
		verb := "pruned"
		if archived {
			verb = "archived"
		}
		fmt.Printf("%s %d stale %s\n", verb, res.Pruned, plural(res.Pruned, "entry", "entries"))
	}
	if len(res.Pending) > 0 {
		fmt.Printf("\n%d marked %s awaiting distillation (run /distill to generalize):\n",
			len(res.Pending), plural(len(res.Pending), "memory", "memories"))
		for _, o := range res.Pending {
			fmt.Printf("  - %s  (%s/%s)\n", o.Name, o.Project, o.File)
		}
	}
	if len(res.Conflicts) > 0 {
		fmt.Printf("\n%d %s — same name, divergent content (resolve in /distill):\n",
			len(res.Conflicts), plural(len(res.Conflicts), "conflict", "conflicts"))
		for _, c := range res.Conflicts {
			fmt.Printf("  - %s: %v\n", c.Name, c.Sources)
		}
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
