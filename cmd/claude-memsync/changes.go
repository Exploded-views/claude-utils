package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/MarimerLLC/claude-utils/internal/changes"
	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/scrub"
	syncpkg "github.com/MarimerLLC/claude-utils/internal/sync"
)

// defaultChangesLimit keeps the output small enough to drop into a prompt.
const defaultChangesLimit = 20

// runChanges implements `claude-memsync changes`. It lists the memory files
// that changed in the sync repo since a point in its history, for Claude Code
// hooks that want to tell a running session its notes moved. It is read-only:
// it never fetches, commits or touches ~/.claude.
func runChanges(args []string) int {
	return changesCmd(os.Stdin, os.Stdout, os.Stderr, args)
}

func changesCmd(stdin io.Reader, stdout, stderr io.Writer, args []string) (code int) {
	if hookRequested(args) {
		// Claude Code treats exit 2 as "block the prompt", which is what an
		// uncaught panic would give.
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(stderr, "changes: internal error: %v\n", r)
				code = 0
			}
		}()
	}
	flags := flag.NewFlagSet("changes", flag.ContinueOnError)
	flags.SetOutput(stderr)
	cfgPath := flags.String("config", "", "path to config.json (default: ~/.claudesync/config.json)")
	since := flags.String("since", "", "git revision to compare HEAD with (this or --marker is required)")
	marker := flags.String("marker", "", "file whose first line is the base commit (this or --since is required)")
	project := flags.String("project", "", "only this project: a project directory, or Claude's folder name for it")
	asJSON := flags.Bool("json", false, "print JSON instead of text")
	limit := flags.Int("limit", defaultChangesLimit, "list at most N changes, then \"...N more\" (0 = no limit)")
	update := flags.Bool("update-marker", false, "with --marker, rewrite the file to the current HEAD afterwards (a missing or stale marker only records the baseline)")
	synced := flags.Bool("synced", false, "compare with the commit last copied into ~/.claude (.state/synced-head) instead of HEAD")
	routesFlag := flags.String("routes", "", "with --hook: path to routes.json, to report across several repos (default: $"+routesEnv+", then ~/.config/claude-memsync/routes.json if it exists)")
	hook := flags.Bool("hook", false, "run as a Claude Code hook: read its JSON from stdin, keep a per-session marker, print additionalContext JSON")
	if err := flags.Parse(args); err != nil {
		if hookRequested(args) {
			return 0 // a hook must never block a prompt
		}
		return 2
	}
	if *hook {
		if flagsSet(flags, "since", "marker", "update-marker", "json", "synced") || *limit < 0 {
			fmt.Fprintln(stderr, "changes: --hook takes only --config, --routes, --project and --limit")
			return 0
		}
		return changesHook(stdin, stdout, stderr, *cfgPath, *routesFlag, *project, *limit)
	}
	if (*since == "") == (*marker == "") {
		fmt.Fprintln(stderr, "changes: exactly one of --since and --marker is required")
		return 2
	}
	if *update && *marker == "" {
		fmt.Fprintln(stderr, "changes: --update-marker only works with --marker")
		return 2
	}
	if *limit < 0 {
		fmt.Fprintln(stderr, "changes: --limit must be 0 or more")
		return 2
	}

	cfg, err := loadDistillConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, "changes:", err)
		return 1
	}
	upper := ""
	if *synced {
		upper = syncedUpper(cfg.SyncDir)
	}
	hide, hideAll := denylistFilter(cfg, stderr)
	res, err := changes.Compute(changes.Options{
		SyncDir:      cfg.SyncDir,
		Upper:        upper,
		Hide:         hide,
		HideAll:      hideAll,
		CompareLocal: true,
		ClaudeDir:    cfg.ClaudeProjectsDir,
		Since:        *since,
		Marker:       *marker,
		Project:      *project,
		Limit:        *limit,
		UpdateMarker: *update,
	})
	if err != nil {
		fmt.Fprintln(stderr, "changes:", err)
		return 1
	}

	if res.Reset {
		fmt.Fprintf(stderr, "changes: marker %s was empty or named a commit that no longer exists; reset it to %s\n", *marker, res.Head)
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			fmt.Fprintln(stderr, "changes:", err)
			return 1
		}
		return 0
	}
	fmt.Fprint(stdout, res.Text())
	return 0
}

// denylistFilter builds the filter that keeps denylisted names out of the
// report, from the same file and parser the daemon uses. A name is hidden when
// its repo path matches. No denylist file: nothing is hidden.
// A file that exists but cannot be read or parsed hides every name (hideAll)
// and costs one line on stderr: it cannot vouch for any of them.
func denylistFilter(cfg config.Config, stderr io.Writer) (hide func(changes.Change) bool, hideAll bool) {
	f, err := os.Open(cfg.DenylistPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false
	}
	var deny []scrub.DenylistPattern
	if err == nil {
		deny, err = scrub.ParseDenylist(f)
		f.Close()
	}
	if err != nil {
		fmt.Fprintf(stderr, "changes: cannot use denylist %s (%v); listing no file names\n", cfg.DenylistPath(), err)
		return nil, true
	}
	sc := scrub.New(deny)
	// The repo path only, the same path the daemon's commit-time check sees:
	// the local path also carries the ~/.claude prefix, and a pattern that
	// happens to match the home directory would hide every change.
	return func(c changes.Change) bool {
		return sc.MatchesPath(c.Path)
	}, false
}

// syncedUpper is the commit the daemon last copied into ~/.claude, for use as
// changes.Options.Upper; "" (so HEAD) when there is no record or it cannot be
// read. Compute checks that the commit exists.
func syncedUpper(syncDir string) string {
	rec, err := syncpkg.SyncedHead(gitwt.New(syncDir))
	if err != nil {
		return ""
	}
	return rec
}

// hookRequested reports whether args ask for hook mode, for the case where
// they did not even parse.
func hookRequested(args []string) bool {
	for _, a := range args {
		if a == "--hook" || a == "-hook" {
			return true
		}
	}
	return false
}

func flagsSet(flags *flag.FlagSet, names ...string) bool {
	set := false
	flags.Visit(func(f *flag.Flag) {
		for _, n := range names {
			if f.Name == n {
				set = true
			}
		}
	})
	return set
}
