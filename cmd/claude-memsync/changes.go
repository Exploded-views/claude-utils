package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/MarimerLLC/claude-utils/internal/changes"
)

// defaultChangesLimit keeps the output small enough to drop into a prompt.
const defaultChangesLimit = 20

// runChanges implements `claude-memsync changes`. It lists the memory files
// that changed in the sync repo since a point in its history, for Claude Code
// hooks that want to tell a running session its notes moved. It is read-only:
// it never fetches, commits or touches ~/.claude.
func runChanges(args []string) int {
	return changesCmd(os.Stdout, os.Stderr, args)
}

func changesCmd(stdout, stderr io.Writer, args []string) int {
	flags := flag.NewFlagSet("changes", flag.ContinueOnError)
	flags.SetOutput(stderr)
	cfgPath := flags.String("config", "", "path to config.json (default: ~/.claudesync/config.json)")
	since := flags.String("since", "", "git revision to compare HEAD with (this or --marker is required)")
	marker := flags.String("marker", "", "file whose first line is the base commit (this or --since is required)")
	project := flags.String("project", "", "only this project: a project directory, or Claude's folder name for it")
	asJSON := flags.Bool("json", false, "print JSON instead of text")
	limit := flags.Int("limit", defaultChangesLimit, "list at most N changes, then \"...N more\" (0 = no limit)")
	update := flags.Bool("update-marker", false, "with --marker, rewrite the file to the current HEAD afterwards (a missing or stale marker only records the baseline)")
	if err := flags.Parse(args); err != nil {
		return 2
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
	res, err := changes.Compute(changes.Options{
		SyncDir:      cfg.SyncDir,
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
		fmt.Fprintf(stderr, "changes: marker %s named a commit that no longer exists; reset it to %s\n", *marker, res.Head)
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
