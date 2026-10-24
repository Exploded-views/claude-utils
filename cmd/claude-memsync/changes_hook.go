package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/changes"
)

// sessionIDPattern is what a session id must match before it is used as a
// file name.
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// hookInput is the part of Claude Code's hook JSON that changesHook uses.
type hookInput struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Event     string `json:"hook_event_name"`
}

// changesHook implements `changes --hook` and `changes --hook-cleanup`. A
// hook that fails or prints garbage can block the user's prompt, so every
// error here is one line on stderr, nothing on stdout, and exit 0.
func changesHook(stdin io.Reader, stdout, stderr io.Writer, cfgPath, project string, limit int, cleanup bool) int {
	fail := func(err error) int {
		fmt.Fprintln(stderr, "changes:", err)
		return 0
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
	marker := filepath.Join(cfg.SyncDir, ".state", "hook-markers", in.SessionID)

	if cleanup {
		if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
			return fail(err)
		}
		return 0
	}
	if in.Event == "" {
		return fail(fmt.Errorf("hook input has no hook_event_name"))
	}
	if project == "" {
		project = in.Cwd
	}
	res, err := changes.Compute(changes.Options{
		SyncDir:      cfg.SyncDir,
		ClaudeDir:    cfg.ClaudeProjectsDir,
		Marker:       marker,
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
	if res.Total > 0 {
		var lines []string
		for _, c := range res.Changes {
			lines = append(lines, c.Status+" "+c.Display())
		}
		if res.More > 0 {
			lines = append(lines, fmt.Sprintf("… and %d more", res.More))
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
	if err := changes.WriteMarker(marker, res.Head); err != nil {
		return fail(err)
	}
	return 0
}
