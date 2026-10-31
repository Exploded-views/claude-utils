package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// hookExpiryWindow is how long expiries share one log line.
const hookExpiryWindow = time.Minute

// hookExpiryMaxLines bounds hook-expiry.log.
const hookExpiryMaxLines = 200

type hookExpiryState struct {
	Since time.Time `json:"since"` // when the current line was started
	Count int       `json:"count"` // expiries counted on it
}

// noteHookExpiry records that a hook ran out of time, so the silence a user
// sees has a trace: <stateDir>/hook-expiry.log gets one line per minute with
// the number of expiries in it (the last line is rewritten while the minute
// runs), and hook-expiry.json keeps when that line started.
func noteHookExpiry(stateDir string, now time.Time) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	statePath := filepath.Join(stateDir, "hook-expiry.json")
	logPath := filepath.Join(stateDir, "hook-expiry.log")
	var st hookExpiryState
	if b, err := os.ReadFile(statePath); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	var lines []string
	if b, err := os.ReadFile(logPath); err == nil {
		lines = strings.Split(strings.TrimRight(string(b), "\n"), "\n")
		if len(lines) == 1 && lines[0] == "" {
			lines = nil
		}
	}
	if len(lines) > 0 && st.Count > 0 && !now.Before(st.Since) && now.Sub(st.Since) < hookExpiryWindow {
		st.Count++
		lines = lines[:len(lines)-1]
	} else {
		st = hookExpiryState{Since: now, Count: 1}
	}
	lines = append(lines, fmt.Sprintf("%s changes hook deadline exceeded, notices deferred (count=%d since %s)",
		now.UTC().Format(time.RFC3339), st.Count, st.Since.UTC().Format(time.RFC3339)))
	if len(lines) > hookExpiryMaxLines {
		lines = lines[len(lines)-hookExpiryMaxLines:]
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	b, _ := json.Marshal(st)
	return os.WriteFile(statePath, b, 0o600)
}

// hookExpired leaves the one-line trace of an expiry in the first repo's state
// dir. The caller killed the hook's git children before this (proc.KillAll),
// and passes the targets the prelude loaded (none if it had not finished).
func hookExpired(targets []hookTarget) {
	if len(targets) > 0 {
		_ = noteHookExpiry(filepath.Join(targets[0].cfg.SyncDir, ".state"), time.Now())
	}
}
