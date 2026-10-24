package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/changes"
	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// sessionIDPattern is what a session id must match before it is used as a
// file name.
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// hookTestHook runs at the start of changesHook; tests set it to inject a panic.
var hookTestHook func()

// hookInput is the part of Claude Code's hook JSON that changesHook uses.
type hookInput struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Event     string `json:"hook_event_name"`
}

// hookFraming opens every additionalContext, so the model reads the list of
// paths as a notice to act on rather than as part of the user's prompt.
const hookFraming = "Memory notes changed in the sync repo (this or another machine) since you last looked (read a file before relying on it):"

// markerMaxAge is how long a session's marker survives without being touched.
const markerMaxAge = 30 * 24 * time.Hour

// changesHook implements `changes --hook`. A hook that fails or prints
// garbage can block the user's prompt, so every error here is one line on
// stderr, nothing on stdout, and exit 0.
func changesHook(stdin io.Reader, stdout, stderr io.Writer, cfgPath, routesFlag, project string, limit int) int {
	fail := func(err error) int {
		fmt.Fprintln(stderr, "changes:", err)
		return 0
	}
	if hookTestHook != nil {
		hookTestHook()
	}
	var in hookInput
	if err := json.NewDecoder(io.LimitReader(stdin, 1<<20)).Decode(&in); err != nil {
		return fail(fmt.Errorf("read hook input: %w", err))
	}
	if !sessionIDPattern.MatchString(in.SessionID) {
		return 0
	}
	targets, err := hookTargets(cfgPath, routesFlag)
	if err != nil {
		return fail(err)
	}

	if in.Event == "" {
		return fail(fmt.Errorf("hook input has no hook_event_name"))
	}
	// Claude's cwd follows its `cd`, so the project it was started in is the
	// better default.
	if project == "" {
		project = os.Getenv("CLAUDE_PROJECT_DIR")
	}
	if project == "" {
		project = in.Cwd
	}

	// One report per repo, each against its own marker. A repo reports only the
	// folders it owns; with one repo that is all of them, as before.
	type repoReport struct {
		cfg    config.Config
		marker string
		res    changes.Result
	}
	var reports []repoReport
	for _, tg := range targets {
		markerDir := filepath.Join(tg.cfg.SyncDir, ".state", "hook-markers")
		marker := filepath.Join(markerDir, in.SessionID)
		pruneMarkers(markerDir, in.SessionID, markerMaxAge)
		hide, hideAll := denylistFilter(tg.cfg, stderr)
		res, err := changes.Compute(changes.Options{
			SyncDir:      tg.cfg.SyncDir,
			ClaudeDir:    tg.cfg.ClaudeProjectsDir,
			Marker:       marker,
			Upper:        syncedUpper(tg.cfg.SyncDir),
			Keep:         tg.keep,
			Hide:         hide,
			HideAll:      hideAll,
			CompareLocal: true,
			Project:      project,
			Limit:        limit,
			UpdateMarker: true,
			DeferMarker:  true,
		})
		if err != nil {
			if len(targets) == 1 {
				return fail(err)
			}
			fmt.Fprintf(stderr, "changes: repo %s: %v\n", tg.name, err)
			continue
		}
		if res.Busy {
			continue
		}
		if res.Reset {
			fmt.Fprintf(stderr, "changes: marker for session %s named a commit that no longer exists; reset it\n", in.SessionID)
		}
		reports = append(reports, repoReport{tg.cfg, marker, res})
	}

	lines := []string{hookFraming}
	reported := false
	for _, r := range reports {
		if r.res.Total == 0 && r.res.Hidden == 0 {
			continue
		}
		reported = true
		for _, c := range r.res.Changes {
			lines = append(lines, c.Status+" "+c.Describe())
		}
		if r.res.More > 0 {
			lines = append(lines, fmt.Sprintf("… and %d more", r.res.More))
		}
		if r.res.Hidden > 0 {
			lines = append(lines, r.res.HiddenLine())
		}
	}
	if reported {
		var reply struct {
			Out struct {
				Event   string `json:"hookEventName"`
				Context string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		reply.Out.Event = in.Event
		reply.Out.Context = strings.Join(lines, "\n")
		if err := json.NewEncoder(stdout).Encode(reply); err != nil {
			return fail(err)
		}
	}
	// Only now that the report went out does a marker move; otherwise the
	// next prompt reports the same changes again.
	for _, r := range reports {
		if err := changes.SaveMarker(gitwt.New(r.cfg.SyncDir), r.marker, r.res); err != nil {
			return fail(err)
		}
	}
	return 0
}

// hookTarget is one repo a hook reports on, and the gate on its folders.
type hookTarget struct {
	name string
	cfg  config.Config
	keep func(changes.Change) bool // nil: every folder
}

// hookTargets is the repos the hook reports on: the one --config names, else
// every repo of routes.json this host has (each limited to its own folders),
// else the default repo.
func hookTargets(cfgPath, routesFlag string) ([]hookTarget, error) {
	path, ok, err := resolveRoutesPath(routesFlag)
	if err != nil {
		return nil, err
	}
	if !ok {
		cfg, err := loadDistillConfig(cfgPath)
		if err != nil {
			return nil, err
		}
		return []hookTarget{{name: "default", cfg: cfg}}, nil
	}
	set, err := loadRouted(path)
	if err != nil {
		return nil, err
	}
	gate := func(n string) func(changes.Change) bool {
		owns := set.Router.Owns(n)
		return func(c changes.Change) bool { return owns(c.Project) }
	}
	if cfgPath != "" {
		// An explicit --config names one repo, but it still reports only the
		// folders routes.json gives that repo, and it must be listed there.
		cfg, err := loadDistillConfig(cfgPath)
		if err != nil {
			return nil, err
		}
		name := set.repoForSyncDir(cfg.SyncDir)
		if name == "" {
			return nil, fmt.Errorf("routes file %s does not list the sync dir %s", path, cfg.SyncDir)
		}
		return []hookTarget{{name, cfg, gate(name)}}, nil
	}
	var out []hookTarget
	for _, n := range set.names() {
		out = append(out, hookTarget{n, set.Cfgs[n], gate(n)})
	}
	return out, nil
}

// pruneMarkers deletes the markers in dir that were last written more than
// maxAge ago, which belong to sessions that never came back. It keeps the
// current session's marker: a resumed session needs it to report what changed
// while it was closed. Errors are ignored; this is housekeeping.
func pruneMarkers(dir, keep string, maxAge time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		if e.Name() == keep || !e.Type().IsRegular() {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
