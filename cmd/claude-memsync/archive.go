package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/MarimerLLC/claude-utils/internal/archive"
	"github.com/MarimerLLC/claude-utils/internal/config"
)

// runArchive implements `claude-memsync archive list|search`. The archive is
// read-only from here: the tool never deletes from it, and a purge is a git
// history rewrite done by hand (see docs: Archive, never delete).
func runArchive(args []string) int { return archiveCmd(os.Stdout, os.Stderr, args) }

const archiveUsage = `usage:
  claude-memsync archive list [--since YYYY-MM-DD] [--config path] [--routes path]
  claude-memsync archive search <term> [--show] [--config path] [--routes path]`

func archiveCmd(stdout, stderr io.Writer, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, archiveUsage)
		return 2
	}
	sub, rest := args[0], args[1:]
	flags := flag.NewFlagSet("archive "+sub, flag.ContinueOnError)
	flags.SetOutput(stderr)
	cfgPath := flags.String("config", "", "path to config.json (default: ~/.claudesync/config.json)")
	routes := flags.String("routes", "", "path to routes.json (default: $"+routesEnv+", then ~/.config/claude-memsync/routes.json if it exists)")
	since := flags.String("since", "", "list: only archive dates on or after this one")
	show := flags.Bool("show", false, "search: print the matching lines, not only paths and line counts")

	switch sub {
	case "list", "search":
	default:
		fmt.Fprintf(stderr, "archive: unknown subcommand %q\n%s\n", sub, archiveUsage)
		return 2
	}
	// The term comes first (`search <term> --show`), so split it off before
	// the flags are parsed.
	term := ""
	if sub == "search" {
		if len(rest) == 0 || len(rest[0]) == 0 || rest[0][0] == '-' {
			fmt.Fprintln(stderr, archiveUsage)
			return 2
		}
		term, rest = rest[0], rest[1:]
	}
	if err := flags.Parse(rest); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintln(stderr, archiveUsage)
		return 2
	}

	targets, err := archiveTargets(*cfgPath, *routes)
	if err != nil {
		fmt.Fprintln(stderr, "archive:", err)
		return 1
	}
	for _, t := range targets {
		prefix := ""
		if len(targets) > 1 {
			prefix = "[" + t.name + "] "
		}
		switch sub {
		case "list":
			entries, err := archive.List(t.syncDir, *since)
			if err != nil {
				fmt.Fprintln(stderr, "archive:", err)
				return 1
			}
			for _, e := range entries {
				fmt.Fprintf(stdout, "%s%s  %s  %s\n", prefix, e.Date, e.Folder, e.File)
			}
		case "search":
			hits, err := archive.Search(t.syncDir, term)
			if err != nil {
				fmt.Fprintln(stderr, "archive:", err)
				return 1
			}
			for _, h := range hits {
				if !*show {
					fmt.Fprintf(stdout, "%s%s  (%d matching %s)\n", prefix, h.Path, len(h.Lines), plural(len(h.Lines), "line", "lines"))
					continue
				}
				fmt.Fprintf(stdout, "%s%s\n", prefix, h.Path)
				for _, l := range h.Lines {
					fmt.Fprintf(stdout, "    %s\n", l)
				}
			}
		}
	}
	return 0
}

type archiveTarget struct{ name, syncDir string }

// archiveTargets are the repos to read: the one named by --config, else every
// repo of the routes file this host has, else the default sync dir. Archives
// are per repo, so each is read on its own.
func archiveTargets(cfgPath, routesFlag string) ([]archiveTarget, error) {
	if cfgPath != "" {
		cfg, err := config.Load(cfgPath)
		if err != nil {
			return nil, err
		}
		return []archiveTarget{{"", cfg.SyncDir}}, nil
	}
	path, ok, err := resolveRoutesPath(routesFlag)
	if err != nil {
		return nil, err
	}
	if ok {
		set, err := loadRouted(path)
		if err != nil {
			return nil, err
		}
		var out []archiveTarget
		for _, n := range set.names() {
			out = append(out, archiveTarget{n, set.Cfgs[n].SyncDir})
		}
		return out, nil
	}
	cfg, err := config.Load(defaultConfigPath())
	if errors.Is(err, fs.ErrNotExist) {
		cfg, err = config.Defaults(), nil
	}
	if err != nil {
		return nil, err
	}
	return []archiveTarget{{"", cfg.SyncDir}}, nil
}
