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
func changesHook(stdin io.Reader, stdout, stderr io.Writer, cfgPath, project string, limit int) int {
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
	cfg, err := loadDistillConfig(cfgPath)
	if err != nil {
		return fail(err)
	}
	markerDir := filepath.Join(cfg.SyncDir, ".state", "hook-markers")
	marker := filepath.Join(markerDir, in.SessionID)

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
	pruneMarkers(markerDir, in.SessionID, markerMaxAge)
	hide, hideAll := denylistFilter(cfg, stderr)
	res, err := changes.Compute(changes.Options{
		SyncDir:      cfg.SyncDir,
		ClaudeDir:    cfg.ClaudeProjectsDir,
		Marker:       marker,
		Upper:        syncedUpper(cfg.SyncDir),
		Hide:         hide,
		HideAll:      hideAll,
		CompareLocal: true,
		Project:      project,
		Limit:        limit,
		UpdateMarker: true,
		DeferMarker:  true,
	})
	if err != nil {
		return fail(err)
	}
	if res.Busy {
		return 0
	}
	if res.Reset {
		fmt.Fprintf(stderr, "changes: marker for session %s named a commit that no longer exists; reset it\n", in.SessionID)
	}
	if res.Total > 0 || res.Hidden > 0 {
		lines := []string{hookFraming}
		for _, c := range res.Changes {
			lines = append(lines, c.Status+" "+c.Describe())
		}
		if res.More > 0 {
			lines = append(lines, fmt.Sprintf("… and %d more", res.More))
		}
		if res.Hidden > 0 {
			lines = append(lines, res.HiddenLine())
		}
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
	// Only now that the report went out does the marker move; otherwise the
	// next prompt reports the same changes again.
	if err := changes.SaveMarker(gitwt.New(cfg.SyncDir), marker, res); err != nil {
		return fail(err)
	}
	return 0
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
