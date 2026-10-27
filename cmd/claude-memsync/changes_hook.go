package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/changes"
	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/route"
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

// defaultHookDeadline is how long the hook may run before it gives up: well
// inside Claude Code's 10 s hook timeout, which discards the output of a hook
// that overruns.
const defaultHookDeadline = 1500 * time.Millisecond

// hookDeadlineEnv overrides defaultHookDeadline, in milliseconds (0: none).
const hookDeadlineEnv = "CLAUDE_MEMSYNC_HOOK_DEADLINE_MS"

// hookDeadlineFromEnv is the deadline $CLAUDE_MEMSYNC_HOOK_DEADLINE_MS names,
// or the default when it is unset or not a non-negative integer.
func hookDeadlineFromEnv() time.Duration {
	ms, err := strconv.Atoi(strings.TrimSpace(os.Getenv(hookDeadlineEnv)))
	if err != nil || ms < 0 {
		return defaultHookDeadline
	}
	return time.Duration(ms) * time.Millisecond
}

// hookWorkDone, if set, runs when the hook's worker goroutine ends; tests use
// it to wait for a worker that outlived the deadline.
var hookWorkDone func()

// changesHook implements `changes --hook`. A hook that fails or prints
// garbage can block the user's prompt, so every error here is one line on
// stderr, nothing on stdout, and exit 0.
//
// The work runs in a goroutine that touches no marker and writes to no shared
// stream. If it is not done within deadline (0: no limit), the hook prints
// nothing, moves nothing and returns; the worker (and any git process it
// started) dies with the process. Only the caller's goroutine prints the
// report and moves markers, and only after the worker finished in time, so a
// late finisher can never move one.
func changesHook(stdin io.Reader, stdout, stderr io.Writer, cfgPath, routesFlag, project string, limit int, deadline time.Duration) int {
	type outcome struct {
		out      []byte
		commit   func() error
		err      error
		panicked bool
		panicVal any
	}
	var errBuf bytes.Buffer // the worker's until it reports back
	done := make(chan outcome, 1)
	go func() {
		var o outcome
		defer func() {
			if r := recover(); r != nil {
				o.panicked, o.panicVal = true, r
			}
			if hookWorkDone != nil {
				hookWorkDone()
			}
			done <- o
		}()
		o.out, o.commit, o.err = hookWork(stdin, &errBuf, cfgPath, routesFlag, project, limit)
	}()
	var expired <-chan time.Time
	if deadline > 0 {
		t := time.NewTimer(deadline)
		defer t.Stop()
		expired = t.C
	}
	select {
	case o := <-done:
		if o.panicked {
			panic(o.panicVal) // changesCmd turns it into one stderr line
		}
		stderr.Write(errBuf.Bytes())
		if o.err != nil {
			fmt.Fprintln(stderr, "changes:", o.err)
			return 0
		}
		if len(o.out) > 0 {
			if _, err := stdout.Write(o.out); err != nil {
				fmt.Fprintln(stderr, "changes:", err)
				return 0
			}
		}
		// Only now that the report went out does a marker move; otherwise the
		// next prompt reports the same changes again.
		if o.commit != nil {
			if err := o.commit(); err != nil {
				fmt.Fprintln(stderr, "changes:", err)
			}
		}
		return 0
	case <-expired:
		fmt.Fprintln(stderr, "changes: hook deadline exceeded; notices deferred to the next prompt")
		return 0
	}
}

// hookWork computes the hook's report. It returns the bytes to print (nil:
// nothing) and a function that moves the markers, which the caller runs once
// the output was written. It moves no marker itself.
func hookWork(stdin io.Reader, stderr io.Writer, cfgPath, routesFlag, project string, limit int) (out []byte, commit func() error, err error) {
	if hookTestHook != nil {
		hookTestHook()
	}
	var in hookInput
	if err := json.NewDecoder(io.LimitReader(stdin, 1<<20)).Decode(&in); err != nil {
		return nil, nil, fmt.Errorf("read hook input: %w", err)
	}
	if !sessionIDPattern.MatchString(in.SessionID) {
		return nil, nil, nil
	}
	targets, err := hookTargets(cfgPath, routesFlag)
	if err != nil {
		return nil, nil, err
	}

	if in.Event == "" {
		return nil, nil, fmt.Errorf("hook input has no hook_event_name")
	}
	// Claude's cwd follows its `cd`, so the project it was started in is the
	// better default.
	if project == "" {
		project = os.Getenv("CLAUDE_PROJECT_DIR")
	}
	if project == "" {
		project = in.Cwd
	}

	alerts, alertCommit := heldAlerts(targets, in.SessionID)

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
				return nil, nil, err
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
	// Read-only clones of the project's related repos: paths only.
	var relLines []string
	var relCommit func()
	if hookHasRelated(routesFlag) {
		var rerr error
		relLines, relCommit, rerr = relatedHookLines(routesFlag, project, in.SessionID, limit, stderr)
		if rerr != nil {
			fmt.Fprintln(stderr, "changes: related:", rerr)
		}
	}
	if len(relLines) > 0 {
		if reported {
			lines = append(lines, "")
		} else {
			lines = nil
		}
		lines = append(lines, "Memory notes changed in related, read-only repos (paths only; read a file before relying on it):")
		lines = append(lines, relLines...)
		reported = true
	}
	if len(alerts) > 0 {
		if reported {
			lines = append(append(alerts, ""), lines...)
		} else {
			lines = alerts
		}
		reported = true
	}
	if !reported {
		out = nil
	} else {
		var reply struct {
			Out struct {
				Event   string `json:"hookEventName"`
				Context string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		reply.Out.Event = in.Event
		reply.Out.Context = strings.Join(lines, "\n")
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(reply); err != nil {
			return nil, nil, err
		}
		out = buf.Bytes()
	}
	commit = func() error {
		alertCommit()
		if relCommit != nil {
			relCommit()
		}
		for _, r := range reports {
			if err := changes.SaveMarker(gitwt.New(r.cfg.SyncDir), r.marker, r.res); err != nil {
				return err
			}
		}
		return nil
	}
	return out, commit, nil
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

// heldAlerts is one context line per repo the private-only check holds (its
// .state/loop-failed is a hold record), so the agent knows that repo is not
// syncing (the design: hold only that repo, and alert the agents). Each
// session hears about a given record once: commit records the record's digest
// in the session's ".held" marker, and runs only after the output went out.
// It reads local files only: no lock, no git, no network.
func heldAlerts(targets []hookTarget, session string) (lines []string, commit func()) {
	type seen struct{ path, digest string }
	var marks []seen
	for _, tg := range targets {
		b, err := os.ReadFile(loopFailedPath(tg.cfg.SyncDir))
		if err != nil || !isHeldRecord(b) {
			continue
		}
		sum := sha256.Sum256(b)
		digest := hex.EncodeToString(sum[:])
		mark := filepath.Join(tg.cfg.SyncDir, ".state", "hook-markers", session+".held")
		if prev, err := os.ReadFile(mark); err == nil && strings.TrimSpace(string(prev)) == digest {
			continue
		}
		when, reason, _ := strings.Cut(strings.TrimSpace(string(b)), " ")
		reason = strings.TrimPrefix(reason, heldMarker)
		lines = append(lines, fmt.Sprintf("memsync: repo %s is HELD by the private-only check since %s: %s. Nothing syncs for it (no pull, commit or push) until the check passes; see `claude-memsync status`.", tg.name, when, reason))
		marks = append(marks, seen{mark, digest})
	}
	return lines, func() {
		for _, m := range marks {
			_ = os.MkdirAll(filepath.Dir(m.path), 0o700)
			_ = os.WriteFile(m.path, []byte(m.digest+"\n"), 0o600)
		}
	}
}

// hookHasRelated reports whether a routes file with a registry is in play.
func hookHasRelated(routesFlag string) bool {
	path, ok, err := resolveRoutesPath(routesFlag)
	if err != nil || !ok {
		return false
	}
	f, err := route.Load(path)
	return err == nil && f.Registry != ""
}
