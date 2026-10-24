package sync

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/distill"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// Loop is the long-running daemon body. It watches the Claude memory tree,
// mirrors file changes into the sync work-tree, and runs git commit/pull/push
// cycles on a debounce.
type Loop struct {
	Cfg      config.Config
	Branch   string
	Hostname string
	OnFlush  func(localChanges bool, err error) // optional, for tests/observability

	// PushAllowed, when set, is asked before each push. Returning false
	// keeps committing and pulling locally but does not push (the remote is
	// no longer private).
	PushAllowed func() bool

	// Name labels this loop's log lines when several repos run in one process.
	Name string
	// Owns gates which Claude folders this loop may touch (see Roots.Owns).
	// nil means all of them: one repo per host.
	Owns func(folder string) bool

	// recovered is the outcome of a recoverLeftovers the cycle already ran
	// before flush, so flush does not repeat it: a path whose restore keeps
	// failing is counted once per cycle.
	recovered *recovery
}

type recovery struct{ err error }

// Run blocks until ctx is canceled. Returns the first fatal error or nil
// when ctx ends. Recoverable errors (transient git failures) are logged.
func (l *Loop) Run(ctx context.Context) error {
	if l.Branch == "" {
		l.Branch = "main"
	}
	if l.Hostname == "" {
		h, _ := os.Hostname()
		l.Hostname = h
	}

	repo := gitwt.New(l.Cfg.SyncDir)
	if !repo.IsRepo() {
		return fmt.Errorf("sync dir %s is not a git repo — run `claude-memsync init` first", l.Cfg.SyncDir)
	}

	mirrorProjects := filepath.Join(l.Cfg.SyncDir, "projects")
	roots := Roots{Claude: l.Cfg.ClaudeProjectsDir, Mirror: mirrorProjects, Owns: l.Owns}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create watcher: %w", err)
	}
	defer watcher.Close()

	if err := os.MkdirAll(roots.Claude, 0700); err != nil {
		return err
	}
	if err := watcher.Add(roots.Claude); err != nil {
		return fmt.Errorf("watch %s: %w", roots.Claude, err)
	}
	if err := refreshMemoryWatches(watcher, roots.Claude); err != nil {
		l.logln("refresh memory watches:", newPathGuard(l.Cfg).Err(err))
	}

	debounce := time.NewTimer(time.Hour)
	if !debounce.Stop() {
		<-debounce.C
	}
	debounceDur := time.Duration(l.Cfg.DebounceMs) * time.Millisecond
	pullTicker := time.NewTicker(time.Duration(l.Cfg.PullIntervalSec) * time.Second)
	defer pullTicker.Stop()

	pendingLocal := false

	// Initial sync at startup: reconcile local with mirror (using manifest
	// to detect deletes that happened while the daemon was off), then flush.
	manifest, err := LoadManifest(l.Cfg.SyncDir)
	if err != nil {
		l.logln("load manifest (continuing without delete detection):", newPathGuard(l.Cfg).Err(err))
		manifest = NewManifest()
	}
	// What a crash left behind is undone before Reconcile compares the trees: a
	// stopped rebase or merge holds conflict markers, and the parked mirror
	// copies of an unfinished park differ from the Claude ones.
	if l.startCycle(repo, roots) {
		if _, err := Reconcile(roots, manifest); err != nil {
			l.logln("initial reconcile:", newPathGuard(l.Cfg).Err(err))
		}
	}
	if err := l.flush(repo, roots, true); err != nil {
		if !errors.Is(err, errHandResolving) { // already logged
			l.logln("initial flush:", newPathGuard(l.Cfg).Err(err))
		}
		if l.OnFlush != nil {
			l.OnFlush(true, err)
		}
	} else if l.OnFlush != nil {
		l.OnFlush(true, nil)
	}
	l.refreshManifest(roots)
	l.rebuildDistilledIndex()

	for {
		select {
		case <-ctx.Done():
			return nil

		case ev, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			handled, err := l.handleEvent(watcher, roots, ev)
			if err != nil {
				l.logln("event:", newPathGuard(l.Cfg).Err(err))
				continue
			}
			if !handled {
				continue
			}
			pendingLocal = true
			debounce.Reset(debounceDur)

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			l.logln("watcher error:", newPathGuard(l.Cfg).Err(err))

		case <-debounce.C:
			err := l.flush(repo, roots, true)
			if err != nil && !errors.Is(err, errHandResolving) {
				l.logln("flush (local):", newPathGuard(l.Cfg).Err(err))
			}
			l.refreshManifest(roots)
			l.rebuildDistilledIndex()
			if l.OnFlush != nil {
				l.OnFlush(true, err)
			}
			pendingLocal = false

		case <-pullTicker.C:
			if pendingLocal {
				continue // debounce will handle it
			}
			l.pullTick(repo, roots)
		}
	}
}

// keyless reports (and logs) that encryption is on but this host's identity
// cannot read the repo.
func (l *Loop) keyless(repo *gitwt.Repo) bool {
	err := agecrypt.CheckKeyed(repo)
	if errors.Is(err, agecrypt.ErrKeyless) {
		log.Println("encryption: keyless, syncing stopped:", err)
		return true
	}
	if err != nil {
		log.Println("encryption: key check failed:", err)
	}
	return false
}

// keylessPull is all a keyless host does in a cycle: fetch and rebase what
// others pushed, which is how the recipients change that admits this host
// arrives. Nothing is committed, propagated to the Claude tree or pushed, and
// the synced head is not recorded, so catchUp copies the new content once the
// host can read it.
func (l *Loop) keylessPull(repo *gitwt.Repo) {
	if handResolving(repo) {
		return
	}
	if err := runPull(repo, l.Branch); err != nil {
		if repo.RebaseInProgress() {
			_ = abortRebaseConflict(repo)
		} else {
			log.Println("pull:", newPathGuard(l.Cfg).Err(err))
		}
	}
	clearPullMark(repo)
}

// recoverLeftovers undoes what a crash or an earlier failed cycle left: a
// rebase or merge the daemon's own pull left stopped, then an unfinished park.
// A rebase or merge the daemon did not start is the user's: it returns
// errHandResolving and nothing else is done. Last, it copies into the
// Claude tree whatever moved the sync repo's HEAD outside a cycle (catchUp).
func (l *Loop) recoverLeftovers(repo *gitwt.Repo, roots Roots) error {
	if err := abortDaemonInterrupted(repo); err != nil {
		if errors.Is(err, errHandResolving) {
			return err
		}
		return fmt.Errorf("abort interrupted rebase or merge: %w", err)
	}
	if err := finishParked(repo, roots); err != nil {
		return fmt.Errorf("finish earlier park: %w", err)
	}
	if err := l.catchUp(repo, roots); err != nil {
		return fmt.Errorf("catch up with the sync repo: %w", err)
	}
	return nil
}

// takeRecovery is recoverLeftovers for flush, unless the cycle already ran it.
func (l *Loop) takeRecovery(repo *gitwt.Repo, roots Roots) error {
	if r := l.recovered; r != nil {
		l.recovered = nil
		return r.err
	}
	err := l.recoverLeftovers(repo, roots)
	if errors.Is(err, errHandResolving) {
		l.logln(err)
	}
	return err
}

// startCycle runs recoverLeftovers ahead of a Reconcile and keeps the result
// for the flush that follows. It reports whether Reconcile may run: not while
// a park is unfinished (the parked mirror copies are not the user's, and
// Reconcile would merge them into the Claude tree), nor while a rebase or
// merge is still stopped.
func (l *Loop) startCycle(repo *gitwt.Repo, roots Roots) bool {
	// A keyless clone holds ciphertext: Reconcile would copy it into the
	// Claude tree.
	if l.keyless(repo) {
		return false
	}
	err := l.recoverLeftovers(repo, roots)
	l.recovered = &recovery{err}
	if errors.Is(err, errHandResolving) {
		l.logln(err)
		return false
	}
	if err != nil {
		l.logln("recover earlier cycle:", newPathGuard(l.Cfg).Err(err))
	}
	if parkedExists(repo) || repo.RebaseInProgress() || repo.MergeInProgress() {
		l.logln("reconcile skipped: held-back files are still parked or a rebase/merge is unresolved; retrying next cycle")
		return false
	}
	return true
}

// pullTick is the periodic cycle: re-walk the Claude tree, then flush.
func (l *Loop) pullTick(repo *gitwt.Repo, roots Roots) {
	// Safety net: re-walk the Claude tree to catch any watcher events
	// missed by races (e.g., a new project dir and memory file created
	// in rapid succession before the new dir's watcher was registered).
	// Using the on-disk manifest also lets us propagate deletes the
	// watcher missed.
	m, err := LoadManifest(l.Cfg.SyncDir)
	if err != nil {
		l.logln("load manifest:", newPathGuard(l.Cfg).Err(err))
		m = NewManifest()
	}
	if l.startCycle(repo, roots) {
		if _, err := Reconcile(roots, m); err != nil {
			l.logln("reconcile:", newPathGuard(l.Cfg).Err(err))
		}
	}
	err = l.flush(repo, roots, true)
	if err != nil && !errors.Is(err, errHandResolving) {
		l.logln("flush (pull):", newPathGuard(l.Cfg).Err(err))
	}
	l.refreshManifest(roots)
	l.rebuildDistilledIndex()
	if l.OnFlush != nil {
		l.OnFlush(false, err)
	}
}

// handleEvent filters and reacts to a single fsnotify event. Returns
// (handled, error). handled=true means the change was relevant and should
// trigger a debounced flush.
func (l *Loop) handleEvent(watcher *fsnotify.Watcher, roots Roots, ev fsnotify.Event) (bool, error) {
	rel, err := filepath.Rel(roots.Claude, ev.Name)
	if err != nil {
		return false, nil
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")

	// Discover newly-created project dirs and start watching their memory subdir.
	if ev.Op&fsnotify.Create != 0 {
		if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
			// either a new <hash>/ dir or a new memory/ inside one we already watched.
			_ = watcher.Add(ev.Name) // best-effort
			_ = refreshMemoryWatches(watcher, roots.Claude)
			return false, nil
		}
	}

	// We only care about files under <hash>/memory/<filename>.
	if len(parts) != 3 || parts[1] != "memory" {
		return false, nil
	}
	hash, name := parts[0], parts[2]
	if shouldIgnoreFile(name) || !roots.owns(hash) {
		return false, nil
	}

	switch {
	case ev.Op&(fsnotify.Create|fsnotify.Write) != 0:
		if err := CopyToMirror(roots, hash, name); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// Race: file vanished between event and read. Treat as remove.
				if err := RemoveFromMirror(roots, hash, name); err != nil {
					return true, newPathGuard(l.Cfg).PathErr("remove from mirror", "projects/"+hash+"/memory/"+name, err)
				}
				return true, nil
			}
			return false, newPathGuard(l.Cfg).PathErr("copy to mirror", "projects/"+hash+"/memory/"+name, err)
		}
		return true, nil

	case ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0:
		if err := RemoveFromMirror(roots, hash, name); err != nil {
			return false, newPathGuard(l.Cfg).PathErr("remove from mirror", "projects/"+hash+"/memory/"+name, err)
		}
		return true, nil
	}
	return false, nil
}

// flush stages local changes, optionally commits, pulls remote work, propagates
// any pull-driven changes back to the Claude tree, and pushes.
//
// Before doing any pull/push, flush asks origin for the current branch SHA
// (a single ls-remote round-trip). If origin's SHA matches our cached
// refs/remotes/origin/<branch> AND we have no unpushed commits, it returns
// early — skipping the full pull/push churn that dominates idle ticks.
func (l *Loop) flush(repo *gitwt.Repo, roots Roots, includeLocal bool) (err error) {
	// Registered first, so it runs last: whatever the cycle returns (including
	// the restore error below) is stripped of denylisted names.
	g := newPathGuard(l.Cfg)
	defer func() { err = g.Err(err) }()
	// Without a key that reads the repo nothing may be committed, pulled into
	// the Claude tree or pushed.
	if l.keyless(repo) {
		l.keylessPull(repo)
		return agecrypt.ErrKeyless
	}
	// A rebase or merge the daemon left stopped goes first (one the user is
	// resolving ends the cycle), before anything is staged, and then
	// a park an earlier cycle (or a crashed run) did not finish: nothing may be
	// committed, or parked over, while files are still away.
	if err := l.takeRecovery(repo, roots); err != nil {
		if errors.Is(err, errHandResolving) {
			markPaused(repo)
			return err // already logged, once for the cycle
		}
		return err
	}
	// The cycle's HEAD is recorded once it is reflected in the Claude tree
	// (see catchUp). A propagate that failed leaves the older record.
	settled := true
	// A cycle that cannot go on says so in its result (and in .state/paused),
	// not as a success.
	pause := func() error {
		settled = false
		markPaused(repo)
		l.logln(errHandResolving)
		return errHandResolving
	}
	defer func() {
		if settled {
			if rerr := recordHead(repo); rerr != nil {
				l.logln("record synced head:", g.Err(rerr))
			}
		}
	}()
	// The check in takeRecovery ran at the start of the cycle, which may have
	// been long ago: the user may have started a rebase since. Staging or
	// committing in the middle of it would corrupt it.
	if handResolving(repo) {
		return pause()
	}
	clearPaused(repo)
	logUnreviewed(repo)
	if includeLocal {
		_, err := CommitMirror(repo, l.Cfg, fmt.Sprintf("auto: %s @ %s", l.Hostname, time.Now().UTC().Format(time.RFC3339)))
		// Unreviewed recipients stop commits and pushes; pulling goes on.
		if err != nil && !errors.Is(err, agecrypt.ErrRecipientsUnreviewed) {
			return fmt.Errorf("commit: %w", err)
		}
	}

	if idle, err := remoteAndLocalIdle(repo, l.Branch); err == nil && idle {
		return nil
	}

	// Whatever CommitMirror left uncommitted (held back by the scrub, or
	// everything when the denylist policy blocks the commit) is parked: reset
	// to HEAD so the pull runs on a clean tree, skipped by propagateChanges,
	// and put back afterwards (see park.go). The deferred finish runs whatever
	// happens below, and tries every path.
	defer func() {
		if ferr := finishParked(repo, roots); ferr != nil {
			err = errors.Join(err, fmt.Errorf("restore held-back files: %w", ferr))
		}
	}()
	// Once more right before the pull, ahead of the park, which resets files.
	beforePullCheck(repo)
	if handResolving(repo) {
		return pause()
	}
	st, err := parkUncommitted(repo)
	if err != nil {
		return fmt.Errorf("park held-back files: %w", err)
	}
	preRev, _ := repo.Run("rev-parse", "HEAD")
	var conflictErr error
	// The marker names the rebase for the reflog, so the next start knows a
	// rebase found stopped is ours to abort; it stays while one is.
	if err := runPull(repo, l.Branch); err != nil {
		if repo.RebaseInProgress() && l.Cfg.ArchiveDeletes {
			// Two hosts archiving one note conflict in a way that needs no
			// decision (see archive_resolve.go); anything else stays stopped.
			n, rerr := resolveArchiveConflicts(repo)
			if n > 0 {
				l.logln(resolvedLine(n))
			}
			if rerr != nil {
				l.logln("pull:", g.Err(rerr))
			}
			if !repo.RebaseInProgress() && rerr == nil {
				err = nil
			}
		}
		if err == nil {
			// resolved: the pull is done
		} else if repo.RebaseInProgress() {
			conflictErr = abortRebaseConflict(repo)
		} else {
			// Don't propagate first-time failure (no upstream yet); push will create it.
			l.logln("pull:", g.Err(err))
		}
	}
	clearPullMark(repo)
	if errors.Is(conflictErr, errHandResolving) {
		return pause()
	}
	postRev, _ := repo.Run("rev-parse", "HEAD")
	if conflictErr == nil && l.Cfg.ArchiveDeletes && strings.TrimSpace(preRev) != strings.TrimSpace(postRev) && strings.TrimSpace(preRev) != "" {
		// An edit that merged into an archive copy is put back (archive_repair.go).
		n, rerr := repairEditedArchives(repo, l.Cfg, strings.TrimSpace(preRev), strings.TrimSpace(postRev))
		if rerr != nil {
			// Never leave a mismatched copy: back to where the cycle began.
			if _, e := repo.Run("reset", "--hard", strings.TrimSpace(preRev)); e != nil {
				rerr = errors.Join(rerr, e)
			}
			conflictErr = fmt.Errorf("pull: could not restore an edited archived note (%s); the pull was undone, local commits kept, nothing propagated or pushed", g.Err(rerr))
		} else if n > 0 {
			l.logln(fmt.Sprintf("pull: restored %d note(s) edited after they were archived (the archive copies are unchanged)", n))
			postRev, _ = repo.Run("rev-parse", "HEAD")
		}
	}
	if conflictErr == nil && strings.TrimSpace(preRev) != strings.TrimSpace(postRev) && strings.TrimSpace(preRev) != "" {
		noteRecipientsChange(repo, strings.TrimSpace(preRev), strings.TrimSpace(postRev))
		if err := propagate(repo, roots, strings.TrimSpace(preRev), strings.TrimSpace(postRev), st.skip(), g); err != nil {
			settled = false
			if !errors.Is(err, errPendingCiphertext) { // not a failure: the rest of the cycle goes on
				return fmt.Errorf("propagate: %w", err)
			}
		}
	}
	if conflictErr != nil {
		return conflictErr // nothing to push: origin is ahead
	}

	if l.PushAllowed != nil && !l.PushAllowed() {
		return nil
	}
	logUnreviewed(repo)
	if err := agecrypt.PushGuard(repo, l.Branch); err != nil {
		if !errors.Is(err, agecrypt.ErrRecipientsUnreviewed) { // logged once by logUnreviewed
			log.Println(err)
		}
		return err
	}
	if err := repo.Push(l.Branch); err != nil {
		return fmt.Errorf("push: %w", err)
	}
	return nil
}

// errPendingCiphertext is propagateChanges' report that some files hold
// ciphertext this host cannot read. They were not copied; the synced head is
// not advanced past them.
var errPendingCiphertext = errors.New("some files hold ciphertext this host cannot read")

// propagate is propagateChanges; a variable so a test can make it fail.
var propagate = propagateChanges

// beforePullCheck runs just before the check for a rebase or merge ahead of the
// pull; a variable so a test can start one there.
var beforePullCheck = func(*gitwt.Repo) {}

// abortRebaseConflict deals with a `pull --rebase` that stopped on a conflict
// (both PCs edited the same lines of a committed file). It aborts the rebase, so
// the work-tree never holds conflict markers for propagateChanges to copy into
// the Claude tree. Nothing decides the conflict for the user: the local commits
// are kept, nothing is propagated or pushed, and every cycle fails the same way
// until the user resolves it (see "Rebase conflicts" in the docs). The error is
// the cycle's one log line; it gives the count only, as the paths may match the
// denylist.
//
// Only a rebase this pull started is aborted (see stoppedByMarkedPull). One the
// user started in the meantime makes the pull fail without touching it; that
// is errHandResolving.
func abortRebaseConflict(repo *gitwt.Repo) error {
	if !stoppedByMarkedPull(repo) {
		return errHandResolving
	}
	conflicted, _ := repo.UnmergedPaths()
	n := len(conflicted)
	if err := repo.RebaseAbort(); err != nil {
		return fmt.Errorf("pull: rebase conflict in %d file(s) and abort failed; resolve in %s", n, repo.Dir)
	}
	return fmt.Errorf("pull: rebase conflict in %d file(s); rebase aborted, local commits kept, nothing propagated or pushed until you resolve it (see docs: Rebase conflicts)", n)
}

// remoteAndLocalIdle reports whether there is nothing to do: origin's branch
// SHA matches our cached remote-tracking ref AND we have no unpushed commits.
// Returns (false, nil) when the gate cannot prove idleness — including the
// first run before refs/remotes/origin/<branch> exists locally.
func remoteAndLocalIdle(repo *gitwt.Repo, branch string) (bool, error) {
	remoteSHA, err := repo.LsRemoteHead(branch)
	if err != nil || remoteSHA == "" {
		return false, err
	}
	cachedSHA, err := repo.Run("rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch)
	if err != nil {
		return false, nil // remote-tracking ref missing → can't prove idle
	}
	if strings.TrimSpace(cachedSHA) != remoteSHA {
		return false, nil // remote moved
	}
	unpushed, err := repo.Run("rev-list", "--count", "refs/remotes/origin/"+branch+"..HEAD")
	if err != nil {
		return false, nil
	}
	return strings.TrimSpace(unpushed) == "0", nil
}

// propagateChanges replays the file changes between pre and post revisions
// from the mirror back into Claude's tree. Paths in skip (parked, so the
// mirror copy is not the user's) are left alone; that test is on the sync-repo
// path and comes before any mapping to a Claude folder.
func propagateChanges(repo *gitwt.Repo, roots Roots, pre, post string, skip map[string]bool, g *pathGuard) error {
	// -z: "<status>\0<path>\0", or "<status>\0<old>\0<new>\0" for a rename
	// or copy. Unlike the line format, it never quotes or splits a name.
	out, err := repo.Run("diff", "--name-status", "-z", "--find-renames", pre+".."+post)
	if err != nil {
		return err
	}
	fields := strings.Split(out, "\x00")
	pending := 0
	defer func() {
		if pending > 0 {
			log.Printf("propagate: %d file(s) hold ciphertext this host cannot read; left pending", pending)
		}
	}()
	for i := 0; i < len(fields); {
		status := fields[i]
		i++
		if status == "" {
			continue
		}
		want := 1
		if status[0] == 'R' || status[0] == 'C' {
			want = 2
		}
		if i+want > len(fields) {
			break
		}
		paths := fields[i : i+want]
		i += want

		// A rename's old path is removed; its new path is the post-image.
		path := paths[want-1]
		if status[0] == 'R' && !skip[paths[0]] {
			if hash, name, ok := splitMirrorPath(paths[0]); ok && roots.owns(hash) {
				_ = os.Remove(filepath.Join(roots.Claude, hash, "memory", name))
			}
		}
		if skip[path] {
			continue
		}
		hash, name, ok := splitMirrorPath(path)
		if !ok || !roots.owns(hash) {
			continue
		}
		switch status[0] {
		case 'D':
			_ = os.Remove(filepath.Join(roots.Claude, hash, "memory", name))
		default:
			if err := CopyToClaude(roots, hash, name); errors.Is(err, ErrCiphertext) {
				pending++
			} else if err != nil {
				log.Printf("propagate %s: %v", g.Path(path), g.Err(err))
			}
		}
	}
	if pending > 0 {
		return errPendingCiphertext
	}
	return nil
}

// splitMirrorPath converts a path of the form "projects/<hash>/memory/<file>"
// (as it appears in `git diff --name-only`) into its parts.
func splitMirrorPath(p string) (hash, name string, ok bool) {
	parts := strings.Split(filepath.ToSlash(p), "/")
	if len(parts) != 4 || parts[0] != "projects" || parts[2] != "memory" {
		return "", "", false
	}
	return parts[1], parts[3], true
}

// refreshMemoryWatches walks the Claude tree and ensures each existing
// <hash>/memory/ directory is watched. Idempotent.
func refreshMemoryWatches(w *fsnotify.Watcher, claudeRoot string) error {
	entries, err := os.ReadDir(claudeRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		mem := filepath.Join(claudeRoot, e.Name(), "memory")
		if info, err := os.Stat(mem); err == nil && info.IsDir() {
			_ = w.Add(mem) // no-op if already watching
		}
	}
	return nil
}

// refreshManifest rebuilds the manifest from the current Claude tree state
// and saves it. Called after every successful flush so that the next
// Reconcile reasons against an up-to-date "as of last sync" snapshot.
func (l *Loop) refreshManifest(roots Roots) {
	m, err := BuildFromClaudeTreeOwned(roots.Claude, roots.Owns)
	if err != nil {
		l.logln("build manifest:", newPathGuard(l.Cfg).Err(err))
		return
	}
	if err := m.Save(l.Cfg.SyncDir); err != nil {
		l.logln("save manifest:", newPathGuard(l.Cfg).Err(err))
	}
}

// rebuildDistilledIndex regenerates the local DISTILLED.md catalog index from
// the synced entry files. The index is a derived, git-ignored artifact, so this
// never affects the sync repo; it just keeps the local index fresh after entries
// arrive (from the /distill skill locally or via a pull). Best-effort: a missing
// catalog dir is skipped and errors are logged, never fatal.
func (l *Loop) rebuildDistilledIndex() {
	dir := l.Cfg.DistilledPath()
	if _, err := os.Stat(dir); err != nil {
		return // no catalog on this PC yet
	}
	if _, err := distill.BuildIndex(distill.Options{
		ProjectsDir:  l.Cfg.ClaudeProjectsDir,
		DistilledDir: dir,
	}); err != nil {
		l.logln("rebuild distilled index:", newPathGuard(l.Cfg).Err(err))
	}
}

// shouldIgnoreFile returns true for filenames the daemon should not sync.
func shouldIgnoreFile(name string) bool {
	if name == "" {
		return true
	}
	if strings.HasPrefix(name, ".") {
		return true
	}
	// Atomic-write temp files: a trailing ".tmp", or the
	// "<name>.tmp.<pid>.<hash>" form left behind by interrupted memory writes
	// (the pid-suffixed variant does not end in ".tmp", so the suffix check
	// alone misses it). Neither is a real memory; they must never sync.
	if strings.HasSuffix(name, ".tmp") || strings.Contains(name, ".tmp.") {
		return true
	}
	return false
}

// logln logs like log.Println, prefixed with the loop's name when it has one,
// so the lines of several repos in one process can be told apart.
func (l *Loop) logln(args ...any) {
	if l.Name != "" {
		args = append([]any{"[" + l.Name + "]"}, args...)
	}
	log.Println(args...)
}
