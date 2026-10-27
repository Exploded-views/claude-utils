package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/foreign"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/lock"
	"github.com/MarimerLLC/claude-utils/internal/route"
	syncpkg "github.com/MarimerLLC/claude-utils/internal/sync"
)

// routesEnv overrides the routes.json location (tests, unusual layouts).
const routesEnv = "CLAUDE_MEMSYNC_ROUTES"

// defaultRoutesPath is routes.json in config.Dir(), per host and never
// synced.
func defaultRoutesPath() string {
	dir, _ := config.Dir()
	return filepath.Join(dir, "routes.json")
}

// knownRoutesPaths lists the places a routes file may be without being named:
// the environment variable's path, then $XDG_CONFIG_HOME's and ~/.config's.
func knownRoutesPaths() []string {
	var out []string
	add := func(p string) {
		if p == "" {
			return
		}
		for _, o := range out {
			if o == p {
				return
			}
		}
		out = append(out, p)
	}
	add(os.Getenv(routesEnv))
	add(defaultRoutesPath())
	home, _ := os.UserHomeDir()
	add(filepath.Join(home, ".config", "claude-memsync", "routes.json"))
	return out
}

// legacyGuard refuses single-repo mode on a host that has a routes file at any
// known place: such a host's folders belong to several repos, and one daemon
// mirroring all of them into one repo would undo the routing.
func legacyGuard(cfg config.Config) error {
	for _, p := range knownRoutesPaths() {
		if _, err := os.Stat(p); err == nil {
			return fmt.Errorf("a routes file exists at %s: run through it (--routes %s) instead of syncing %s on its own", p, p, cfg.SyncDir)
		}
	}
	return nil
}

// resolveRoutesPath picks the routes file: the flag, then the environment, then
// the default location if a file is there. ok is false for single-repo mode.
// A flag or environment path that does not exist is an error, not a fallback:
// a typo must not silently mirror everything into one repo.
func resolveRoutesPath(flagVal string) (path string, ok bool, err error) {
	explicit := flagVal
	if explicit == "" {
		explicit = os.Getenv(routesEnv)
	}
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", false, fmt.Errorf("routes file: %w", err)
		}
		return explicit, true, nil
	}
	for _, p := range knownRoutesPaths() {
		if _, err := os.Stat(p); err == nil {
			return p, true, nil
		}
	}
	return "", false, nil
}

// routedSet is a loaded routes.json: the router, plus the config of every repo
// this host has.
type routedSet struct {
	reloadErr string // last reload failure, logged once
	Path      string
	File      route.File
	Router    *route.Router
	Cfgs      map[string]config.Config // repos this host has
	Missing   []string                 // repos named in routes.json that this host lacks

	fmu     sync.Mutex
	fglobal []foreign.Writer // routes.json foreignWriters, replaced on reload
	fheld   map[string]bool  // repos paused because a foreign writer was seen; sticky until restart
	finit   bool             // the first recheck of this daemon has cleared old markers
	logf    func(string, ...any)
}

// writersFor is the foreign writers that bear on a repo: the routes file's list
// and the repo's own.
func (s *routedSet) writersFor(name string) []foreign.Writer {
	s.fmu.Lock()
	defer s.fmu.Unlock()
	out := append([]foreign.Writer{}, s.fglobal...)
	return append(out, s.Cfgs[name].ForeignWriters...)
}

// held reports a repo paused because a foreign writer is active.
func (s *routedSet) held(name string) bool {
	s.fmu.Lock()
	defer s.fmu.Unlock()
	return s.fheld[name]
}

// foreignHeldPath is the marker a held repo leaves for `status`, which runs in
// another process. A daemon restart removes it.
func foreignHeldPath(syncDir string) string {
	return filepath.Join(syncDir, ".state", "foreign-held")
}

// recheckForeign asks again, for every repo, whether a foreign writer is
// active. The first sighting pauses the repo, logged once, and the pause is
// sticky: it lasts until the daemon restarts, because the copies the writer
// left behind would be mirrored the moment it stopped. A check that cannot be
// answered changes nothing. The daemon does not exit: other repos keep running.
func (s *routedSet) recheckForeign() {
	s.fmu.Lock()
	first := !s.finit
	s.finit = true
	s.fmu.Unlock()
	for _, name := range s.names() {
		if first { // a restart: whatever an earlier daemon held is over
			_ = os.Remove(foreignHeldPath(s.Cfgs[name].SyncDir))
		}
		if s.held(name) {
			continue
		}
		r := foreign.Inspect(s.writersFor(name), foreignEnv())
		s.fmu.Lock()
		if s.fheld == nil {
			s.fheld = map[string]bool{}
		}
		logf := s.logf
		if logf == nil {
			logf = log.Printf
		}
		switch {
		case len(r.Active) > 0:
			s.fheld[name] = true
			logf("[%s] foreign writer seen: restart after cleanup (%s); nothing is mirrored, committed or pushed for this repo until the daemon restarts", name, r.Active[0])
			p := foreignHeldPath(s.Cfgs[name].SyncDir)
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err == nil {
				_ = os.WriteFile(p, []byte(time.Now().UTC().Format(time.RFC3339)+" foreign writer seen: restart after cleanup ("+r.Active[0].String()+")\n"), 0o600)
			}
		case len(r.Unchecked) > 0:
			logf("[%s] %s could not be checked; keeping the repo running", name, r.Unchecked[0])
		}
		s.fmu.Unlock()
	}
}

// foreignLines is the per-repo part of status for repos a foreign writer holds.
func (s *routedSet) foreignLines() []string {
	var out []string
	for _, name := range s.names() {
		for _, a := range foreign.Check(s.writersFor(name), foreignEnv()) {
			out = append(out, fmt.Sprintf("  [%s] blocked: %s", name, a))
		}
		if b, err := os.ReadFile(foreignHeldPath(s.Cfgs[name].SyncDir)); err == nil {
			msg := strings.TrimSpace(string(b))
			if _, rest, ok := strings.Cut(msg, " "); ok {
				msg = rest // drop the timestamp
			}
			out = append(out, fmt.Sprintf("  [%s] held: %s", name, msg))
		}
	}
	return out
}

func loadRouted(path string) (*routedSet, error) {
	f, err := route.Load(path)
	if err != nil {
		return nil, err
	}
	s := &routedSet{Path: path, File: f, Cfgs: map[string]config.Config{}, fglobal: f.ForeignWriters}
	for _, name := range f.RepoNames() {
		cp := route.ExpandHome(f.Repos[name].Config)
		cfg, err := loadConfig(cp)
		switch {
		case err == nil:
			if err := checkKindAgainstRemote(name, f.Repos[name].Kind, cfg.RemoteURL); err != nil {
				return nil, err
			}
			s.Cfgs[name] = cfg
		case errors.Is(err, fs.ErrNotExist):
			s.Missing = append(s.Missing, name)
		default:
			return nil, fmt.Errorf("repo %q: %w", name, err)
		}
	}
	seen := map[string]string{}
	for _, name := range f.RepoNames() {
		if cfg, ok := s.Cfgs[name]; ok {
			if other, dup := seen[cfg.SyncDir]; dup {
				return nil, fmt.Errorf("repos %q and %q share the sync dir %s", other, name, cfg.SyncDir)
			}
			seen[cfg.SyncDir] = name
		}
	}
	s.Router = route.New(f, func(repo string) bool { _, ok := s.Cfgs[repo]; return ok })
	s.Router.StatePath = filepath.Join(filepath.Dir(path), "owners.json")
	return s, nil
}

// checkKindAgainstRemote refuses a repo whose remote is named like a client or
// orchestrator repo (mem-client-*, mem-orch-*) but whose kind says otherwise:
// that mistake would let a client's notes into the personal catalog.
func checkKindAgainstRemote(name, kind, remote string) error {
	base := strings.TrimSuffix(strings.TrimRight(remote, "/"), ".git")
	if i := strings.LastIndexAny(base, "/:\\"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.ToLower(base)
	for prefix, want := range map[string]string{"mem-client-": route.KindClient, "mem-orch-": route.KindOrch} {
		if strings.HasPrefix(base, prefix) && kind != want {
			return fmt.Errorf("repo %q: its remote is named %s... so its kind must be %q, not %q", name, prefix, want, kind)
		}
	}
	return nil
}

// repoForSyncDir names the repo of routes.json whose config.json lives in dir.
func (s *routedSet) repoForSyncDir(dir string) string {
	want, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for _, name := range s.File.RepoNames() {
		got, err := filepath.Abs(filepath.Dir(route.ExpandHome(s.File.Repos[name].Config)))
		if err == nil && filepath.Clean(got) == filepath.Clean(want) {
			return name
		}
	}
	return ""
}

// Primary is the config the daemon's PID file and the default commands use:
// the personal repo if this host has it, else the first repo by name.
func (s *routedSet) Primary() (config.Config, bool) {
	if c, ok := s.Cfgs[s.File.Personal]; ok && s.File.Personal != "" {
		return c, true
	}
	for _, n := range s.File.RepoNames() {
		if c, ok := s.Cfgs[n]; ok {
			return c, true
		}
	}
	return config.Config{}, false
}

// names lists the repos this host has, sorted.
func (s *routedSet) names() []string {
	var out []string
	for n := range s.Cfgs {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// remotes lists the remote of every repo this host has, in name order: what
// doctor must check as joined.
func (s *routedSet) remotes() []string {
	var out []string
	for _, n := range s.names() {
		if u := s.Cfgs[n].RemoteURL; u != "" {
			out = append(out, u)
		}
	}
	return out
}

// running is a started daemon body: Stop cancels it and waits.
type running struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
	locks  []*lock.Lock
	loop   *syncpkg.Loop   // set for a single-repo run
	loops  []*syncpkg.Loop // set for a routed run
	mu     sync.Mutex
	err    error
}

func (r *running) fail(err error) {
	r.mu.Lock()
	r.err = errors.Join(r.err, err)
	r.mu.Unlock()
}

// Stop ends every loop, waits for them and drops the locks.
func (r *running) Stop() error {
	r.cancel()
	r.wg.Wait()
	for _, l := range r.locks {
		l.Release()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// Wait blocks until every loop has ended.
func (r *running) Wait() { r.wg.Wait() }

// executable is os.Executable; tests replace it.
var executable = os.Executable

// configuredExe is the program a clone's git config names in key (its first,
// shell-quoted word), or "".
func configuredExe(repo *gitwt.Repo, key string) string {
	out, err := repo.Run("config", "--get", key)
	if err != nil {
		return ""
	}
	v := strings.TrimSpace(out)
	if !strings.HasPrefix(v, "'") {
		return ""
	}
	var b strings.Builder
	for i := 1; i < len(v); i++ {
		switch {
		case strings.HasPrefix(v[i:], `'\''`):
			b.WriteByte('\'')
			i += 3
		case v[i] == '\'':
			return strings.ReplaceAll(b.String(), "%%", "%")
		default:
			b.WriteByte(v[i])
		}
	}
	return ""
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// tempRoots are the directories whose binaries count as throwaways.
var tempRoots = func() []string { return []string{os.TempDir(), "/tmp"} }

// underTemp reports a path in a temp dir or a go build cache: a binary there is
// a throwaway, not something to point a clone's git config at.
func underTemp(p string) bool {
	p = filepath.ToSlash(p)
	if strings.Contains(p, "/go-build") {
		return true
	}
	for _, d := range tempRoots() {
		d = strings.TrimRight(filepath.ToSlash(d), "/")
		if d != "" && (p == d || strings.HasPrefix(p, d+"/")) {
			return true
		}
	}
	return false
}

var refreshWarned sync.Map

func warnRefreshOnce(dir, msg string) {
	if _, dup := refreshWarned.LoadOrStore(dir+msg, true); !dup {
		log.Printf("encryption: %s", msg)
	}
}

// refreshEncryption updates an encrypted clone's attributes, filters and merge
// drivers to this build's, at daemon start. Unencrypted repos are left alone.
// git runs whatever programs the clone's config names, so when they are not
// this build (another version, or gone) all of memsync's entries are repointed
// to the running build, by the stable link when there is one, and one line says
// so. A same-version build at another path is kept. A running build under a
// temp dir is a throwaway: the programs the clone names are kept while they
// exist, a missing one is not replaced, and nothing is rewritten.
func refreshEncryption(cfg config.Config) error {
	repo := gitwt.New(cfg.SyncDir)
	if !repo.IsRepo() || !agecrypt.Configured(repo) {
		return nil
	}
	curMS := configuredExe(repo, "filter."+agecrypt.FilterName+".process")
	curMM := configuredExe(repo, "merge.claude-memory-index.driver")
	ms, mm := curMS, curMM
	if b, ok := runningBinaries(cfg); ok {
		msStale := staleExe(curMS, b.ms, b.msRes, ownVersion(b.msRes))
		mmStale := b.mm != "" && staleExe(curMM, b.mm, b.mmRes, ownVersion(b.mmRes))
		if msStale || mmStale {
			// Only a different or missing build is news; a move to the stable
			// spelling of the same build is silent.
			if !fileExists(curMS) || resolveReal(curMS) != b.msRes {
				logRepoint(cfg.SyncDir, curMS, b)
			}
			ms = b.ms
			if b.mm != "" {
				mm = b.mm
			}
		}
	} else if ms == "" || !fileExists(ms) {
		if exe, err := executable(); err == nil && underTemp(exe) {
			warnRefreshOnce(cfg.SyncDir, "the configured claude-memsync is missing and this one runs from a temp dir; encryption settings left as they are")
			return nil
		} else if err != nil {
			return err
		} else {
			ms = exe
		}
	}
	if mm == "" || !fileExists(mm) {
		mm = cfg.MergeDriverPath
		var err error
		if mm == "" {
			if mm, err = config.DiscoverMergeDriver(); err != nil {
				return err
			}
		}
		if underTemp(mm) {
			warnRefreshOnce(cfg.SyncDir, "the configured claude-memmerge is missing and the replacement is in a temp dir; encryption settings left as they are")
			return nil
		}
	}
	return agecrypt.Refresh(repo, ms, mm)
}

// startSingle runs one Loop on cfg, as before routing existed, but refuses a
// second daemon on the same sync dir. A repo that fails the private-only
// check is held until it passes (see awaitPrivate).
func startSingle(ctx context.Context, cfg config.Config) (*running, error) {
	if err := legacyGuard(cfg); err != nil {
		return nil, err
	}
	l, err := lock.Acquire(cfg.SyncDir)
	if err != nil {
		return nil, err
	}
	if err := refreshEncryption(cfg); err != nil {
		l.Release()
		return nil, fmt.Errorf("refresh encryption settings: %w", err)
	}
	first := guardCheck(cfg, log.Printf)
	ctx, cancel := context.WithCancel(ctx)
	loop := &syncpkg.Loop{Cfg: cfg}
	if first == nil {
		clearHeld(cfg.SyncDir)
		loop.PushAllowed = pushWatch(cfg, log.Printf)
	}
	r := &running{cancel: cancel, locks: []*lock.Lock{l}, loop: loop}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		if first != nil {
			if !awaitPrivate(ctx, "", cfg, first) {
				return
			}
			loop.PushAllowed = pushWatch(cfg, log.Printf)
		}
		if err := runLoop(ctx, loop); err != nil {
			r.fail(err)
		}
	}()
	return r, nil
}

// Start takes the lock of every sync dir this host has (all or nothing), then
// runs one Loop per repo, each gated by the router. One loop failing does not
// stop the others.
func (s *routedSet) Start(ctx context.Context) (*running, error) {
	if len(s.Cfgs) == 0 {
		return nil, fmt.Errorf("%s: this host has none of the repos", s.Path)
	}
	s.Router.Writable()
	// The private-only guard runs for every repo before its loop starts. A
	// repo that fails it is held (see awaitPrivate); the others sync meanwhile.
	unverified := map[string]error{}
	for _, name := range s.names() {
		if err := guardCheck(s.Cfgs[name], log.Printf); err != nil {
			unverified[name] = err
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	r := &running{cancel: cancel}
	for _, name := range s.names() {
		l, err := lock.Acquire(s.Cfgs[name].SyncDir)
		if err != nil {
			for _, h := range r.locks {
				h.Release()
			}
			cancel()
			return nil, fmt.Errorf("repo %q: %w", name, err)
		}
		r.locks = append(r.locks, l)
	}
	for _, name := range s.names() {
		if err := refreshEncryption(s.Cfgs[name]); err != nil {
			for _, h := range r.locks {
				h.Release()
			}
			cancel()
			return nil, fmt.Errorf("repo %q: refresh encryption settings: %w", name, err)
		}
	}
	s.publishExpiries()
	for _, m := range s.Missing {
		log.Printf("routing: repo %q is not on this host; folders routed to it are denied", m)
	}
	s.recheckForeign()
	interval := time.Hour
	for _, name := range s.names() {
		cfg := s.Cfgs[name]
		if d := time.Duration(cfg.PullIntervalSec) * time.Second; d > 0 && d < interval {
			interval = d
		}
		loop := &syncpkg.Loop{Cfg: cfg, Name: name, Owns: s.Router.Owns(name), Hold: func() bool { return s.held(name) }}
		first := unverified[name]
		if first == nil {
			clearHeld(cfg.SyncDir)
			loop.PushAllowed = pushWatch(cfg, log.Printf)
		}
		r.loops = append(r.loops, loop)
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			if first != nil {
				if !awaitPrivate(ctx, name, cfg, first) {
					return
				}
				// Built only now: a push guard made while the check failed
				// could have fallen back to the local checks alone.
				loop.PushAllowed = pushWatch(cfg, log.Printf)
			}
			superviseLoop(ctx, loop, s.recheckForeign)
		}()
	}
	// Checkouts and remotes change; decisions are re-read on the pull tick.
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.reload()
				s.Router.Refresh()
				s.syncRelatedInBackground()
				s.refreshSuggestionsInBackground()
			}
		}
	}()
	return r, nil
}

// reload re-reads routes.json and swaps the table in atomically. A file that no
// longer parses leaves the old table in force and is logged once.
func (s *routedSet) reload() {
	defer s.recheckForeign()
	f, err := route.Load(s.Path)
	if err != nil {
		if msg := err.Error(); msg != s.reloadErr {
			s.reloadErr = msg
			log.Printf("routing: keeping the previous routes, %v", err)
		}
		return
	}
	s.reloadErr = ""
	s.fmu.Lock()
	s.fglobal = f.ForeignWriters
	s.fmu.Unlock()
	s.Router.Replace(f)
}

// Private-only hold. A repo whose private-only check fails at start is held:
// its loop does not start, .state/loop-failed (which `status` shows) says why,
// and the check is retried with a growing delay until it passes. Every failure
// holds, a lookup error as much as a public repo, and so does a configured
// token source that gives no token (daemonGuard); nothing reads an error as
// private. This keeps one unverifiable repo from stopping the others, and a
// service manager from restarting the daemon every few seconds.
// heldMarker starts the reason in .state/loop-failed for a held repo.
const heldMarker = "private-only hold: "

var (
	guardBackoffMin = 10 * time.Second
	guardBackoffMax = 30 * time.Minute
	guardCheck      = daemonGuard
	guardWait       = func(ctx context.Context, d time.Duration) bool {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return true
		}
	}
)

// awaitPrivate holds a repo whose check failed with first, re-running the
// check until it passes (true) or ctx ends (false).
func awaitPrivate(ctx context.Context, name string, cfg config.Config, first error) bool {
	failed := loopFailedPath(cfg.SyncDir)
	prefix := ""
	if name != "" {
		prefix = "[" + name + "] "
	}
	backoff := guardBackoffMin
	for err := first; err != nil; err = guardCheck(cfg, log.Printf) {
		msg := heldMarker + "check failed, nothing syncs until it passes: " + strings.SplitN(err.Error(), "\n", 2)[0]
		log.Printf("%s%s; retrying in %s", prefix, msg, backoff)
		_ = os.MkdirAll(filepath.Dir(failed), 0700)
		// Replaced by rename, so status never reads a half-written record.
		tmp := failed + ".tmp"
		if os.WriteFile(tmp, []byte(time.Now().UTC().Format(time.RFC3339)+" "+msg+"\n"), 0600) == nil {
			_ = os.Rename(tmp, failed)
		}
		if !guardWait(ctx, backoff) || ctx.Err() != nil {
			return false
		}
		if backoff *= 2; backoff > guardBackoffMax {
			backoff = guardBackoffMax
		}
	}
	if ctx.Err() != nil {
		return false // the check passed after Stop: start nothing
	}
	os.Remove(failed)
	log.Printf("%sprivate-only check passes; syncing", prefix)
	return true
}

// clearHeld removes a held marker that a stopped daemon left behind, once a
// start's first check passes. A dead loop's record is left to superviseLoop.
func clearHeld(syncDir string) {
	p := loopFailedPath(syncDir)
	if b, err := os.ReadFile(p); err == nil && strings.Contains(string(b), heldMarker) {
		os.Remove(p)
	}
}

// Loop supervision. A loop that dies is recorded in .state/loop-failed (which
// `status` shows) and restarted with a growing delay.
var (
	loopBackoffMin = 5 * time.Second
	loopBackoffMax = 5 * time.Minute
	loopHealthy    = 30 * time.Second
	runLoop        = func(ctx context.Context, l *syncpkg.Loop) error { return l.Run(ctx) }
)

func loopFailedPath(syncDir string) string { return filepath.Join(syncDir, ".state", "loop-failed") }

// loopFailedLine is the status line for a loop-failed record: a repo held by
// the private-only check, or a loop that died.
func loopFailedLine(b []byte) string {
	if strings.Contains(string(b), heldMarker) {
		return "waiting for the private-only check, re-checking with backoff: " + strings.TrimSpace(string(b))
	}
	return "loop stopped, restarting with backoff: " + strings.TrimSpace(string(b))
}

func superviseLoop(ctx context.Context, loop *syncpkg.Loop, recheck func()) {
	superviseLoopWith(ctx, loop, recheck, runLoop, loopBackoffMin)
}

// superviseLoopWith is superviseLoop with the loop function and the first
// backoff given, so a test can supervise a fake loop beside a running set
// without assigning the package variables under a goroutine that reads them.
func superviseLoopWith(ctx context.Context, loop *syncpkg.Loop, recheck func(), run func(context.Context, *syncpkg.Loop) error, backoffMin time.Duration) {
	backoff := backoffMin
	failed := loopFailedPath(loop.Cfg.SyncDir)
	for {
		if recheck != nil {
			recheck() // a restart is a moment to look again for a foreign writer
		}
		healthy := time.AfterFunc(loopHealthy, func() { os.Remove(failed) })
		started := time.Now()
		err := run(ctx, loop)
		healthy.Stop()
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("loop returned")
		}
		msg := strings.SplitN(err.Error(), "\n", 2)[0]
		log.Printf("[%s] stopped: %s; restarting in %s", loop.Name, msg, backoff)
		_ = os.MkdirAll(filepath.Dir(failed), 0700)
		_ = os.WriteFile(failed, []byte(time.Now().UTC().Format(time.RFC3339)+" "+msg+"\n"), 0600)
		if time.Since(started) > loopHealthy {
			backoff = backoffMin
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > loopBackoffMax {
			backoff = loopBackoffMax
		}
	}
}

// claudeFolders lists the project folders in the Claude tree.
func claudeFolders(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

// statusLines describes the routing: each repo, how many folders it owns, how
// many are denied, and the rules an earlier rule shadows.
func (s *routedSet) statusLines() []string {
	lines := []string{"routing: " + s.Path}
	primary, _ := s.Primary()
	sum := s.Router.Summarize(claudeFolders(primary.ClaudeProjectsDir))
	for _, name := range s.Router.File().RepoNames() {
		cfg, have := s.Cfgs[name]
		switch {
		case !have:
			lines = append(lines, fmt.Sprintf("  %s: not on this host (folders routed to it are denied)", name))
		case name == s.Router.File().Personal && sum.Owned[name] == 0:
			lines = append(lines, fmt.Sprintf("  %s: personal, owns no folders; syncs %s", name, cfg.SyncDir))
		default:
			lines = append(lines, fmt.Sprintf("  %s: owns %d %s; syncs %s", name, sum.Owned[name], plural(sum.Owned[name], "folder", "folders"), cfg.SyncDir))
		}
		if have {
			if b, err := os.ReadFile(loopFailedPath(cfg.SyncDir)); err == nil {
				lines = append(lines, "    "+loopFailedLine(b))
			}
			if l := pausedLine(cfg); l != "" {
				lines = append(lines, "    "+strings.ReplaceAll(l, "\n", " "))
			}
		}
	}
	lines = append(lines, fmt.Sprintf("  denied: %d %s", sum.Denied, plural(sum.Denied, "folder", "folders")))
	for _, rf := range sum.Renamed {
		lines = append(lines, fmt.Sprintf("  renamed remote: add an alias (folder %s is routed by its root commits for %d more %s)", rf.Hash, rf.DaysLeft, plural(rf.DaysLeft, "day", "days")))
	}
	for _, h := range sum.Expired {
		lines = append(lines, fmt.Sprintf("  renamed remote: folder %s is denied, the grace period is over; add an alias", h))
	}
	for _, h := range sum.Transferred {
		lines = append(lines, fmt.Sprintf("  renamed remote: client folder %s is denied, its remote moved to another owner; add an alias if that is intended", h))
	}
	for _, h := range sum.Different {
		lines = append(lines, fmt.Sprintf("  different repository at this path: folder %s is denied", h))
	}
	if sum.Unreadable {
		lines = append(lines, "  identity.json is unreadable: folders routed before are denied until it is fixed or removed ("+s.Router.IdentityProblem()+")")
	}
	for _, h := range sum.Changed {
		lines = append(lines, fmt.Sprintf("  remote changed repos: folder %s is denied until routes.json is edited", h))
	}
	for _, sh := range s.Router.Shadowed() {
		lines = append(lines, fmt.Sprintf("  shadowed: rule %d is never reached first (rule %d matches the same folders)", sh.Rule, sh.By))
	}
	return lines
}

// publishExpiries publishes the expiry of this host into the registry, if a
// registry is configured. Best effort: a failure is logged, never fatal.
func (s *routedSet) publishExpiries() {
	if s.File.Registry == "" {
		return
	}
	dir := route.ExpandHome(s.File.Registry)
	ro := false
	for _, n := range s.names() {
		if c := s.Cfgs[n]; c.ReadOnly && sameDir(c.SyncDir, dir) {
			ro = true
		}
	}
	done := map[string]bool{}
	for _, n := range s.names() {
		c := s.Cfgs[n]
		if c.ExpiresAt == "" || done[c.HostLabel+c.ExpiresAt] {
			continue
		}
		done[c.HostLabel+c.ExpiresAt] = true
		if err := publishExpiry(c, dir, ro, log.Printf); err != nil {
			log.Printf("routing: %v", err)
		}
	}
}
