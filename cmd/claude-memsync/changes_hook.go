package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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

// hookRepoTestHook, if set, runs in each repo's goroutine just before its
// Compute; tests use it to hold a repo back.
var hookRepoTestHook func(name string)

// hookKilledTestHook, if set, runs on the deadline path right after the git
// children were killed; tests use it to widen the window in which a killed
// repo's goroutine finishes.
var hookKilledTestHook func()

// hookWorkDone, if set, runs when the hook's worker goroutine ends; tests use
// it to wait for a worker that outlived the deadline.
var hookWorkDone func()

// changesHook implements `changes --hook`. A hook that fails or prints
// garbage can block the user's prompt, so every error here is one line on
// stderr, nothing on stdout, and exit 0.
//
// The work runs in a goroutine that touches no marker and writes to no shared
// stream. It records each repo once that repo's report is complete. If it is
// not done within deadline (0: no limit), the hook first freezes that record
// (a repo killed mid-step is never in it), kills every git process it started
// (by process group), logs the expiry, and prints in one reply the held-repo
// alerts, which were read before any git work, and the notices of the repos
// that finished. Only after that reply was written does it move markers: the
// held ".held" markers and, with no git, the change marker of each finished
// repo. Repos that did not finish keep their markers and are redone on the
// next prompt, as are the related repos; a failed write moves nothing. Git
// stays refused, and the worker starts no new repo, until the worker ended.
// Only the caller's goroutine prints the report and moves markers, so a late
// finisher can never move one.
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
	gen := proc.GroupGen()
	exited := make(chan struct{}) // closed when the worker goroutine has ended
	cut := false
	defer func() {
		if !cut {
			proc.ReleaseGen(gen)
			return
		}
		// A cut leaves the worker running (it may be held in something that is
		// not git): git stays refused until it has ended, so nothing it still
		// does can start a process. The process normally exits first.
		go func() { <-exited; proc.ReleaseGen(gen) }()
	}()
	var errBuf bytes.Buffer // the worker's until it reports back
	done := make(chan outcome, 1)
	pre := make(chan hookPre, 1) // the held-repo alerts, before any git work
	prog := &hookProgress{}      // what the worker has finished, for the deadline path
	go func() {
		var o outcome
		defer func() {
			if r := recover(); r != nil {
				o.panicked, o.panicVal = true, r
			}
			if hookWorkDone != nil {
				hookWorkDone()
			}
			close(exited)
			done <- o
		}()
		buf, rerr := io.ReadAll(io.LimitReader(stdin, 1<<20))
		var p hookPre
		if rerr == nil {
			p = hookPrelude(buf, cfgPath, routesFlag, project)
		}
		pre <- p
		o.out, o.commit, o.release, o.err = hookWork(bytes.NewReader(buf), &errBuf, cfgPath, routesFlag, project, limit, p, prog)
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
		cut = true
		// Freeze the record of finished repos first, so a repo whose git is
		// killed below (and whose last step swallows the error) is never added.
		done, lk := prog.close()
		defer lk.Release()
		// No git outlives the deadline: kill it before any file I/O below.
		proc.KillAll()
		if hookKilledTestHook != nil {
			hookKilledTestHook()
		}
		// The prelude is git-free, so it has practically always finished; if
		// not, its alerts and targets are not available and only the kill and
		// the message remain.
		var p hookPre
		select {
		case p = <-pre:
		default:
		}
		hookExpired(p.targets)
		// What finished is reported with the held-repo alerts (read before any
		// git work); related repos are left to the next prompt.
		reports := make([]repoReport, len(done))
		for i, d := range done {
			reports[i] = d.rep
		}
		lines, reported := hookLines(p.alerts, reports, nil)
		wrote := true
		if reported {
			b, err := hookReplyJSON(p.event, lines)
			if err == nil {
				_, err = stdout.Write(b)
			}
			wrote = err == nil
		}
		// Only after the reply went out (or when there was none to send) do
		// markers move, so a failed write repeats everything next time.
		if wrote {
			if len(p.alerts) > 0 && p.commit != nil {
				p.commit()
			}
			for _, d := range done {
				if _, err := changes.SaveMarkerNoGit(d.rep.marker, d.rep.res); err != nil {
					fmt.Fprintln(stderr, "changes:", err)
				}
			}
		}
		fmt.Fprintln(stderr, "changes: hook deadline exceeded; notices deferred to the next prompt")
		return 0
	}
}

// hookRepoParallel is how many repos the hook works on at once. Unbounded,
// they all finish together, and a deadline cuts every one of them; bounded,
// they finish in a steady stream and each call settles a prefix. The cost is
// fork and exec latency, not CPU, so it is not NumCPU.
const hookRepoParallel = 8

// repoReport is one repo's finished report.
type repoReport struct {
	cfg    config.Config
	marker string
	res    changes.Result
}

type indexedReport struct {
	idx int // position in the targets
	rep repoReport
}

// hookProgress is what the worker has finished, for the deadline path: a repo
// is added only once its Compute returned (not busy, no error). close is
// called once, by the caller's goroutine, after proc.KillAll; after it, add
// refuses and the hook lock belongs to the caller.
type hookProgress struct {
	mu      sync.Mutex
	closed  bool
	lk      *lock.Lock
	lkTaken bool // close took lk; the worker must not release it
	done    []indexedReport
}

// setLock records the hook lock the worker holds. It is false if close already
// ran: the worker then releases the lock itself and stops.
func (p *hookProgress) setLock(lk *lock.Lock) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.lk = lk
	return true
}

// add records a finished repo; it is false (and nothing is recorded) after close.
func (p *hookProgress) add(i int, r repoReport) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.done = append(p.done, indexedReport{i, r})
	return true
}

// close freezes the progress: it returns what finished, in target order, and
// the hook lock (nil if the worker held none), which is now the caller's.
func (p *hookProgress) close() (done []indexedReport, lk *lock.Lock) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	if p.lk != nil {
		p.lkTaken, lk = true, p.lk
	}
	sort.Slice(p.done, func(a, b int) bool { return p.done[a].idx < p.done[b].idx })
	return append([]indexedReport(nil), p.done...), lk
}

// isClosed reports whether close ran: the deadline passed.
func (p *hookProgress) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// releaseIfOwned is the worker's release of its lock: a no-op once close took it.
func (p *hookProgress) releaseIfOwned(lk *lock.Lock) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.lkTaken {
		lk.Release()
	}
}

// hookLines is the hook's context: the held-repo alerts first, then the notices
// of the repos in reports, then (if any) those of the related repos. reported
// is false when there is nothing to say. The in-time and the deadline reply
// both build their lines here, so they cannot drift apart.
func hookLines(alerts []string, reports []repoReport, relLines []string) (lines []string, reported bool) {
	lines = []string{hookFraming}
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
	return lines, reported
}

// hookWork computes the hook's report. It returns the bytes to print (nil:
// nothing) and a function that moves the markers, which the caller runs once
// the output was written. It moves no marker itself.
func hookWork(stdin io.Reader, stderr io.Writer, cfgPath, routesFlag, project string, limit int, p hookPre, prog *hookProgress) (out []byte, commit func() error, release func(), err error) {
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
	targets, err := p.targets, p.terr
	if !p.loaded { // the prelude did not get to it (an unreadable input)
		var set *routedSet
		targets, set, err = hookTargetsSet(cfgPath, routesFlag)
		p.set = set
	}
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
	if lk != nil && !prog.setLock(lk) {
		lk.Release() // the deadline passed while we waited for the lock
		return nil, nil, nil, nil
	}
	owned := false
	defer func() {
		if !owned {
			prog.releaseIfOwned(lk)
		}
	}()

	// One report per repo, each against its own marker. A repo reports only the
	// folders it owns; with one repo that is all of them, as before.
	// The repos are independent (own marker, own git), so they run side by side;
	// each keeps its stderr lines, joined in target order.
	type targetOut struct {
		rep  *repoReport
		errs bytes.Buffer
		err  error
	}
	outs := make([]targetOut, len(targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, hookRepoParallel)
	for i, tg := range targets {
		sem <- struct{}{} // in target order: the first repos get the first slots
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			o := &outs[i]
			if hookRepoTestHook != nil {
				hookRepoTestHook(tg.name)
			}
			if prog.isClosed() { // the deadline passed: start nothing new
				return
			}
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
			prog.add(i, *o.rep)
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

	// Read-only clones of the project's related repos: paths only.
	var relLines []string
	var relCommit func()
	if p.set != nil && p.set.File.Registry != "" {
		var rerr error
		relLines, relCommit, rerr = relatedHookLines(p.set, project, in.SessionID, limit, stderr)
		if rerr != nil {
			fmt.Fprintln(stderr, "changes: related:", rerr)
		}
	}
	lines, reported := hookLines(alerts, reports, relLines)
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

// loadHookConfig is the config the hook reports on, without the sensitive keys
// (see loadConfigHook); a missing file gives the defaults.
func loadHookConfig(cfgPath string) (config.Config, error) {
	if cfgPath == "" {
		cfgPath = defaultConfigPath()
	}
	cfg, err := loadConfigHook(cfgPath)
	if errors.Is(err, fs.ErrNotExist) {
		return config.Defaults(), nil
	}
	return cfg, err
}

// hookTargets is the repos the hook reports on: the one --config names, else
// every repo of routes.json this host has (each limited to its own folders),
// else the default repo. Their configs hold none of the keys the trust check
// guards: the report path never uses them, so it need not judge the file, which
// would cost 4 git processes per repo per load.
func hookTargets(cfgPath, routesFlag string) ([]hookTarget, error) {
	targets, _, err := hookTargetsSet(cfgPath, routesFlag)
	return targets, err
}

// hookTargetsSet is hookTargets that also returns the routes set it read (nil
// when no routes file is in play), so the hook loads the routes and the configs
// once and the related-repos check works from the same load.
func hookTargetsSet(cfgPath, routesFlag string) ([]hookTarget, *routedSet, error) {
	path, ok, err := resolveRoutesPath(routesFlag)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		cfg, err := loadHookConfig(cfgPath)
		if err != nil {
			return nil, nil, err
		}
		return []hookTarget{{name: "default", cfg: cfg}}, nil, nil
	}
	set, err := loadRoutedWith(path, loadConfigHook)
	if err != nil {
		return nil, nil, err
	}
	gate := func(n string) func(changes.Change) bool {
		owns := set.Router.Owns(n)
		return func(c changes.Change) bool { return owns(c.Project) }
	}
	if cfgPath != "" {
		// An explicit --config names one repo, but it still reports only the
		// folders routes.json gives that repo, and it must be listed there.
		cfg, err := loadHookConfig(cfgPath)
		if err != nil {
			return nil, nil, err
		}
		name := set.repoForSyncDir(cfg.SyncDir)
		if name == "" {
			return nil, nil, fmt.Errorf("routes file %s does not list the sync dir %s", path, cfg.SyncDir)
		}
		return []hookTarget{{name, cfg, gate(name)}}, set, nil
	}
	var out []hookTarget
	for _, n := range set.names() {
		out = append(out, hookTarget{n, set.Cfgs[n], gate(n)})
	}
	return out, set, nil
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

	// What the prelude loaded, so the worker, the related-repos check and the
	// deadline path do not load it again: the hook reads routes.json and each
	// config once. loaded is false when the prelude stopped before that.
	loaded  bool
	targets []hookTarget
	set     *routedSet // nil: no routes file
	terr    error
}

// hookPrelude reads the hook input and the held-repo alerts. Any problem gives
// no alerts; the worker reports the input error itself.
func hookPrelude(in []byte, cfgPath, routesFlag, project string) hookPre {
	p := hookPre{commit: func() {}}
	var h hookInput
	if json.Unmarshal(in, &h) != nil || !sessionIDPattern.MatchString(h.SessionID) || h.Event == "" {
		return p
	}
	targets, set, err := hookTargetsSet(cfgPath, routesFlag)
	p.loaded, p.targets, p.set, p.terr = true, targets, set, err
	if err != nil {
		return p
	}
	p.event = h.Event
	p.alerts, p.commit = heldAlerts(targets, h.SessionID)
	// The same project the worker resolves.
	if project == "" {
		project = os.Getenv("CLAUDE_PROJECT_DIR")
	}
	if project == "" {
		project = h.Cwd
	}
	hbLines, hbCommit := heldBackAlerts(targets, h.SessionID, project)
	if len(hbLines) > 0 {
		p.alerts = append(p.alerts, hbLines...)
		held := p.commit
		p.commit = func() { held(); hbCommit() }
	}
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

// heldBackNamesMax caps how many held notes one notice names.
const heldBackNamesMax = 10

// heldBackRecordMax bounds how much of the daemon's held-back record is read.
const heldBackRecordMax = 64 << 10

// heldBackAlerts tells the session which notes of its own project the daemon's
// scrub is not syncing, by file name only. The daemon writes the paths to
// .state/held-back when it holds them back (sync.holdBack), so this reads one
// local file per repo: no git, no lock. A name that itself matches the
// denylist is the private term, so it is counted, not listed; another
// project's held notes are not mentioned. A session hears a given list once;
// commit records it in the session's ".heldback" marker, after the output went
// out.
func heldBackAlerts(targets []hookTarget, session, project string) (lines []string, commit func()) {
	type seen struct{ path, digest string }
	var marks []seen
	prefix := "projects/" + changes.ProjectFolder(project) + "/memory/"
	for _, tg := range targets {
		b, ok := readStateCapped(filepath.Join(tg.cfg.SyncDir, ".state", "held-back"), heldBackRecordMax)
		if !ok {
			continue
		}
		var tl []string // this repo's lines
		rec := parseHeldRecord(b)
		if rec.blocked {
			// The daemon commits nothing here, and any list it left is stale.
			tl = append(tl, "memsync is committing nothing in this repo: its denylist is missing or unreadable (see `claude-memsync status`); no notes sync until it is fixed")
		} else {
			sc, hideAll := denylistScanner(tg.cfg, io.Discard)
			// A denylist that was there when the record was written and is gone
			// now cannot say which names are the private term: count them all.
			if sc == nil && rec.hadDenylist {
				hideAll = true
			}
			var names []string
			hidden := 0
			for _, rel := range rec.paths {
				name, ok := strings.CutPrefix(rel, prefix)
				if !ok || name == "" {
					continue
				}
				if hideAll || (sc != nil && sc.MatchesPath(rel)) {
					hidden++
					continue
				}
				names = append(names, oneLine(name, 80))
			}
			more := hidden
			if len(names) > heldBackNamesMax {
				more += len(names) - heldBackNamesMax
				names = names[:heldBackNamesMax]
			}
			switch {
			case len(names) > 0:
				line := "memsync is NOT syncing these notes because they are held back (denylist or secret rule); rewrite them without the private term (or move them to the right private store): " + strings.Join(names, ", ")
				if more > 0 {
					line += fmt.Sprintf(" (and %d more, not named)", more)
				}
				tl = append(tl, line)
			case more > 0:
				tl = append(tl, fmt.Sprintf("memsync is NOT syncing %d notes of this project because they are held back (denylist or secret rule) and their names match it, so they are not named; rename them or move them to the right private store", more))
			}
			if rec.nameHeld > 0 {
				tl = append(tl, fmt.Sprintf("memsync is NOT syncing %d notes in this repo because their names match your denylist (not named); rename them or move them to the right private store", rec.nameHeld))
			}
		}
		if len(tl) == 0 {
			continue
		}
		line := strings.Join(tl, "\n")
		sum := sha256.Sum256([]byte(line))
		digest := hex.EncodeToString(sum[:])
		mark := filepath.Join(tg.cfg.SyncDir, ".state", "hook-markers", session+".heldback")
		if prev, ok := readHeldRecord(mark); ok && strings.TrimSpace(string(prev)) == digest {
			continue
		}
		lines = append(lines, tl...)
		marks = append(marks, seen{mark, digest})
	}
	return lines, func() {
		for _, m := range marks {
			_ = os.MkdirAll(filepath.Dir(m.path), 0o700)
			writeMarker(m.path, m.digest+"\n")
		}
	}
}

// heldRecord is the daemon's .state/held-back (see sync.heldRecordRel).
type heldRecord struct {
	paths       []string
	nameHeld    int
	hadDenylist bool
	blocked     bool
}

func parseHeldRecord(b []byte) heldRecord {
	var r heldRecord
	for _, l := range strings.Split(string(b), "\n") {
		switch {
		case l == "":
		case l == "#blocked":
			r.blocked = true
		case l == "#denylist":
			r.hadDenylist = true
		case strings.HasPrefix(l, "#name-held "):
			r.nameHeld, _ = strconv.Atoi(strings.TrimPrefix(l, "#name-held "))
		case strings.HasPrefix(l, "#"):
		default:
			r.paths = append(r.paths, l)
		}
	}
	return r
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
func readHeldRecord(p string) (b []byte, ok bool) { return readStateCapped(p, heldRecordMax) }

// readStateCapped is readHeldRecord with the cap given.
func readStateCapped(p string, max int64) (b []byte, ok bool) {
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
	b, err = io.ReadAll(io.LimitReader(f, max))
	return b, err == nil
}
