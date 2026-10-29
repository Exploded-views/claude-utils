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
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/MarimerLLC/claude-utils/internal/changes"
	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/lock"
	"github.com/MarimerLLC/claude-utils/internal/proc"
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
// stream. If it is not done within deadline (0: no limit), the hook first
// kills every git process it started (by process group) and logs the expiry,
// then prints only the held-repo alerts, which were read before any git work,
// and moves only their ".held" markers; change notices are deferred to the
// next prompt and no change marker moves. Only the caller's goroutine prints
// the report and moves markers, and only after the worker finished in time, so
// a late finisher can never move one.
func changesHook(stdin io.Reader, stdout, stderr io.Writer, cfgPath, routesFlag, project string, limit int, deadline time.Duration) int {
	type outcome struct {
		out      []byte
		commit   func() error
		release  func() // drops the hook lock, once the markers moved or the output failed
		err      error
		panicked bool
		panicVal any
	}
	// git children lead their own group, so the deadline can kill them
	proc.Group()
	defer proc.Release()
	var errBuf bytes.Buffer // the worker's until it reports back
	done := make(chan outcome, 1)
	pre := make(chan hookPre, 1) // the held-repo alerts, before any git work
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
		buf, rerr := io.ReadAll(io.LimitReader(stdin, 1<<20))
		var p hookPre
		if rerr == nil {
			p = hookPrelude(buf, cfgPath, routesFlag)
		}
		pre <- p
		o.out, o.commit, o.release, o.err = hookWork(bytes.NewReader(buf), &errBuf, cfgPath, routesFlag, project, limit, p)
	}()
	var expired <-chan time.Time
	if deadline > 0 {
		t := time.NewTimer(deadline)
		defer t.Stop()
		expired = t.C
	}
	select {
	case o := <-done:
		if o.release != nil {
			defer o.release()
		}
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
		// No git outlives the deadline: kill it before any file I/O below.
		hookExpired(cfgPath, routesFlag)
		// Held-repo alerts were read before any git work: they still go out.
		select {
		case p := <-pre:
			if len(p.alerts) > 0 {
				if b, err := hookReplyJSON(p.event, p.alerts); err == nil {
					if _, err := stdout.Write(b); err == nil {
						p.commit()
					}
				}
			}
		default:
		}
		fmt.Fprintln(stderr, "changes: hook deadline exceeded; notices deferred to the next prompt")
		return 0
	}
}

// hookWork computes the hook's report. It returns the bytes to print (nil:
// nothing) and a function that moves the markers, which the caller runs once
// the output was written. It moves no marker itself.
func hookWork(stdin io.Reader, stderr io.Writer, cfgPath, routesFlag, project string, limit int, p hookPre) (out []byte, commit func() error, release func(), err error) {
	if hookTestHook != nil {
		hookTestHook()
	}
	var in hookInput
	if err := json.NewDecoder(io.LimitReader(stdin, 1<<20)).Decode(&in); err != nil {
		return nil, nil, nil, fmt.Errorf("read hook input: %w", err)
	}
	if !sessionIDPattern.MatchString(in.SessionID) {
		return nil, nil, nil, nil
	}
	targets, err := hookTargets(cfgPath, routesFlag)
	if err != nil {
		return nil, nil, nil, err
	}

	if in.Event == "" {
		return nil, nil, nil, fmt.Errorf("hook input has no hook_event_name")
	}
	// Claude's cwd follows its `cd`, so the project it was started in is the
	// better default.
	if project == "" {
		project = os.Getenv("CLAUDE_PROJECT_DIR")
	}
	if project == "" {
		project = in.Cwd
	}

	alerts, alertCommit := p.alerts, p.commit
	if alertCommit == nil {
		alertCommit = func() {}
	}

	// Hooks that pile up (a slow prompt, many sessions) must not each do the
	// work: one at a time, the others return at once and report on the next
	// prompt. The lock lives in the first repo's state dir and is held until the
	// markers moved.
	var lk *lock.Lock
	if len(targets) > 0 {
		var held bool
		if lk, held, err = lock.TryHook(targets[0].cfg.SyncDir); held {
			// Held alerts need no lock (per-session, git-free): tell them now.
			if len(alerts) == 0 {
				return nil, nil, nil, nil
			}
			b, rerr := hookReplyJSON(in.Event, alerts)
			if rerr != nil {
				return nil, nil, nil, rerr
			}
			return b, func() error { alertCommit(); return nil }, nil, nil
		}
	}
	owned := false
	defer func() {
		if !owned {
			lk.Release()
		}
	}()

	// One report per repo, each against its own marker. A repo reports only the
	// folders it owns; with one repo that is all of them, as before.
	type repoReport struct {
		cfg    config.Config
		marker string
		res    changes.Result
	}
	// The repos are independent (own marker, own git), so they run side by side;
	// each keeps its stderr lines, joined in target order.
	type targetOut struct {
		rep  *repoReport
		errs bytes.Buffer
		err  error
	}
	outs := make([]targetOut, len(targets))
	var wg sync.WaitGroup
	for i, tg := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o := &outs[i]
			markerDir := filepath.Join(tg.cfg.SyncDir, ".state", "hook-markers")
			marker := filepath.Join(markerDir, in.SessionID)
			pruneMarkers(markerDir, in.SessionID, markerMaxAge)
			hide, hideAll := denylistFilter(tg.cfg, &o.errs)
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
				o.err = err
				return
			}
			if res.Busy {
				return
			}
			if res.Reset {
				fmt.Fprintf(&o.errs, "changes: marker for session %s named a commit that no longer exists; reset it\n", in.SessionID)
			}
			o.rep = &repoReport{tg.cfg, marker, res}
		}()
	}
	wg.Wait()
	var reports []repoReport
	for i, tg := range targets {
		o := &outs[i]
		stderr.Write(o.errs.Bytes())
		if o.err != nil {
			if len(targets) == 1 {
				return nil, nil, nil, o.err
			}
			fmt.Fprintf(stderr, "changes: repo %s: %v\n", tg.name, o.err)
			continue
		}
		if o.rep != nil {
			reports = append(reports, *o.rep)
		}
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
		if out, err = hookReplyJSON(in.Event, lines); err != nil {
			return nil, nil, nil, err
		}
	}
	owned = true
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
	return out, commit, lk.Release, nil
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

// hookPre is what the hook works out before any git work: the held-repo
// alerts (and how to record them as told), so they survive a deadline.
type hookPre struct {
	event  string
	alerts []string
	commit func()
}

// hookPrelude reads the hook input and the held-repo alerts. Any problem gives
// no alerts; the worker reports the input error itself.
func hookPrelude(in []byte, cfgPath, routesFlag string) hookPre {
	p := hookPre{commit: func() {}}
	var h hookInput
	if json.Unmarshal(in, &h) != nil || !sessionIDPattern.MatchString(h.SessionID) || h.Event == "" {
		return p
	}
	targets, err := hookTargets(cfgPath, routesFlag)
	if err != nil {
		return p
	}
	p.event = h.Event
	p.alerts, p.commit = heldAlerts(targets, h.SessionID)
	return p
}

// hookReplyJSON is the hook's stdout: additionalContext for the event.
func hookReplyJSON(event string, lines []string) ([]byte, error) {
	var reply struct {
		Out struct {
			Event   string `json:"hookEventName"`
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	reply.Out.Event = event
	reply.Out.Context = strings.Join(lines, "\n")
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(reply); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// heldReasonMax caps the reason an alert quotes from a state file.
const heldReasonMax = 200

// oneLine makes state-file text safe to put in the agent's context: control
// and format characters (newlines, bidi controls, zero-width characters) and
// invalid bytes become spaces, runs of spaces (U+2028/2029 included) collapse,
// and it is cut to max runes.
func oneLine(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == utf8.RuneError {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
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
		b, ok := readHeldRecord(loopFailedPath(tg.cfg.SyncDir))
		if !ok || !isHeldRecord(b) {
			continue
		}
		sum := sha256.Sum256(b)
		digest := hex.EncodeToString(sum[:])
		mark := filepath.Join(tg.cfg.SyncDir, ".state", "hook-markers", session+".held")
		if prev, ok := readHeldRecord(mark); ok && strings.TrimSpace(string(prev)) == digest {
			continue
		}
		when, reason, _ := strings.Cut(string(b), " ")
		reason = strings.TrimPrefix(reason, heldMarker)
		lines = append(lines, fmt.Sprintf("memsync: repo %s is HELD by the private-only check since %s: %s. Nothing syncs for it (no pull, commit or push) until the check passes; see `claude-memsync status`.", oneLine(tg.name, 80), oneLine(when, 40), oneLine(reason, heldReasonMax)))
		marks = append(marks, seen{mark, digest})
	}
	return lines, func() {
		for _, m := range marks {
			_ = os.MkdirAll(filepath.Dir(m.path), 0o700)
			writeMarker(m.path, m.digest+"\n")
		}
	}
}

// writeMarker replaces p with data through a fresh temp file in the same
// directory (os.CreateTemp: O_EXCL with a random name, so no planted link is
// followed) and a rename. Errors leave the old marker; the alert repeats.
func writeMarker(p, data string) {
	f, err := os.CreateTemp(filepath.Dir(p), filepath.Base(p)+".*.tmp")
	if err != nil {
		return
	}
	_, werr := f.WriteString(data)
	if cerr := f.Close(); werr != nil || cerr != nil || os.Rename(f.Name(), p) != nil {
		_ = os.Remove(f.Name())
	}
}

// heldRecordMax bounds how much of a loop-failed record the hook reads.
const heldRecordMax = 4 << 10

// readHeldRecord reads at most heldRecordMax bytes of a loop-failed record,
// and only from a regular file: a symlink is not followed and a FIFO does not
// block (openStateFlags), so a stray file can neither hang the hook nor grow
// its memory. ok is false for anything else or any error.
func readHeldRecord(p string) (b []byte, ok bool) {
	if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
		return nil, false
	}
	f, err := os.OpenFile(p, os.O_RDONLY|openStateFlags, 0)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, false
	}
	b, err = io.ReadAll(io.LimitReader(f, heldRecordMax))
	return b, err == nil
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
