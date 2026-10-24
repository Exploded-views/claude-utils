package sync

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// pullMarkerFile is written to .state before the rebase of the daemon's own
// pull and removed after it. It holds a random nonce, and the daemon starts the
// rebase with GIT_REFLOG_ACTION set to "memsync-pull <nonce>", which git
// records in HEAD's reflog as "memsync-pull <nonce> (start): checkout ...". A
// rebase found stopped is the daemon's only if the newest rebase start in the
// reflog carries the marker's nonce. One without a marker, or whose start has
// another name (a rebase you ran by hand) is yours, however the timing falls:
// that covers a marker a crash left behind, and a rebase you start by hand
// while the daemon is fetching or after it.
const pullMarkerFile = "pull-running"

func pullMarkerPath(repo *gitwt.Repo) string {
	return filepath.Join(repo.Dir, manifestStateDir, pullMarkerFile)
}

// pullAction is the GIT_REFLOG_ACTION of the rebase that nonce marks.
func pullAction(nonce string) string { return "memsync-pull " + nonce }

// markPull records that the rebase of a pull is about to start, and returns the
// nonce that names it in the reflog.
func markPull(repo *gitwt.Repo) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	nonce := hex.EncodeToString(b[:])
	return nonce, writeSynced(pullMarkerPath(repo), []byte(nonce+"\n"))
}

func pullMarked(repo *gitwt.Repo) bool {
	_, err := os.Stat(pullMarkerPath(repo))
	return err == nil
}

// reflogDepth is how many reflog entries are searched for the start of a
// rebase: the picks of a long rebase come after it.
const reflogDepth = 2000

// runPull is `git pull --rebase --autostash` as the daemon runs it, in two
// steps: the fetch, then the marker, then the rebase named by the marker's
// nonce. The marker is left for the caller to clear (see clearPullMark).
//
// With noRenames the rebase does not pair deletes with adds. A move to archive/
// is a rename to git, and two hosts that archive the same note differently
// would otherwise stop on a rename/delete conflict; as a delete plus an add the
// commits merge. (Edits made on one host to a note another host renamed no
// longer follow the rename: they conflict, as in any text merge.)
func runPull(repo *gitwt.Repo, branch string, noRenames ...bool) error {
	if err := repo.Fetch(branch); err != nil {
		return err
	}
	afterFetch(repo)
	nonce, err := markPull(repo)
	if err != nil {
		return fmt.Errorf("mark pull: %w", err)
	}
	var confs []string
	if len(noRenames) > 0 && noRenames[0] {
		confs = append(confs, "-s", "recursive", "-Xno-renames") // ort ignores the option in a rebase
	}
	return repo.RebaseOnto(branch, pullAction(nonce), confs...)
}

// afterFetch runs between the fetch and the marker; a variable so a test can
// start a rebase there.
var afterFetch = func(*gitwt.Repo) {}

// stoppedByMarkedPull reports whether a rebase is stopped and the daemon's
// marked pull started it: the newest rebase start in HEAD's reflog is the one
// that carries the marker's nonce. A merge is never the daemon's, as its pull
// is a rebase. Without a marker nothing qualifies.
func stoppedByMarkedPull(repo *gitwt.Repo) bool {
	b, err := os.ReadFile(pullMarkerPath(repo))
	nonce := strings.TrimSpace(string(b))
	if err != nil || nonce == "" || !repo.RebaseInProgress() || repo.MergeInProgress() {
		return false
	}
	subjects, err := repo.ReflogSubjects(reflogDepth)
	if err != nil {
		return false
	}
	for _, s := range subjects {
		if isRebaseStart(s) {
			return strings.HasPrefix(s, pullAction(nonce)+" (start): ")
		}
	}
	return false
}

// isRebaseStart reports whether a reflog subject is a rebase's start entry,
// "<action> (start): checkout <upstream>". Only the part before the first
// "): " counts, so a replayed commit whose own subject contains " (start): "
// ("<action> (pick): notes (start): kickoff") is not taken for one.
func isRebaseStart(subject string) bool {
	head, _, ok := strings.Cut(subject, "): ")
	return ok && strings.HasSuffix(head+")", " (start)")
}

// ClearStalePullMarker removes the pull marker of the sync repo in dir unless a
// rebase or merge is stopped there: `claude-memsync stop` calls it once the
// daemon is down, so a marker a kill left cannot outlive the pull it was for.
// With one stopped it stays, for the next start to judge.
func ClearStalePullMarker(dir string) error {
	repo := gitwt.New(dir)
	if !repo.IsRepo() || handResolving(repo) {
		return nil
	}
	if err := os.Remove(pullMarkerPath(repo)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// clearPullMark removes the marker, unless a rebase or merge the pull started
// is still stopped: that one is still the daemon's to abort.
func clearPullMark(repo *gitwt.Repo) {
	if stoppedByMarkedPull(repo) {
		return
	}
	_ = os.Remove(pullMarkerPath(repo))
}

// errHandResolving ends a cycle early: a rebase or merge is stopped in the sync
// repo that is not recognised as the daemon's own.
var errHandResolving = errors.New("a rebase or merge is in progress in the sync repo that is not recognised as the daemon's own; assuming you are resolving it by hand, this cycle stages, reconciles, pulls and pushes nothing until it is finished (to give it up: git -C ~/.claudesync rebase --abort, or merge --abort; see docs: Rebase conflicts)")

// handResolving reports whether a rebase or merge is stopped in the sync repo.
// Where the daemon checks it, nothing of its own can be: what its pull left
// was aborted at the start of the cycle.
func handResolving(repo *gitwt.Repo) bool {
	return repo.RebaseInProgress() || repo.MergeInProgress()
}

// abortDaemonInterrupted is abortInterrupted for what the daemon's own pull
// left. A stopped rebase or merge that the marked pull did not start is the
// user's, and is left alone: it returns errHandResolving.
func abortDaemonInterrupted(repo *gitwt.Repo) error {
	if handResolving(repo) && !stoppedByMarkedPull(repo) {
		// A marker here is a stale one, from a pull that ended without
		// clearing it; it must not claim a rebase started later.
		_ = os.Remove(pullMarkerPath(repo))
		return errHandResolving
	}
	if err := abortInterrupted(repo); err != nil {
		return err
	}
	_ = os.Remove(pullMarkerPath(repo)) // a marker left by a pull that finished
	return nil
}

// abortInterrupted abandons a rebase or merge that was left stopped, which is
// what a crash (or a kill) in the middle of a pull leaves behind. The daemon
// calls it through abortDaemonInterrupted, at startup and at the start of every
// flush, whether or not anything is parked, but only for its own pull:
// the work-tree of a stopped rebase or merge holds conflict markers, and the
// next commit would stage them. Aborting restores the branch and the tree to
// where the operation found them; the local commits are kept.
func abortInterrupted(repo *gitwt.Repo) error {
	var errs []error
	if repo.RebaseInProgress() {
		if err := repo.RebaseAbort(); err != nil {
			errs = append(errs, fmt.Errorf("abort rebase: %w", err))
		}
	}
	if repo.MergeInProgress() {
		if err := repo.MergeAbort(); err != nil {
			errs = append(errs, fmt.Errorf("abort merge: %w", err))
		}
	}
	return errors.Join(errs...)
}

// conflictMarkers matches the lines git writes around a conflict.
var conflictMarkers = regexp.MustCompile(`(?m)^(?:<{7} |={7}\r?$|>{7} )`)

// refuseConflictMarkers is a safety net for CommitMirror. While a merge or a
// rebase is unresolved, or the index holds unmerged entries, a work-tree file
// with conflict marker lines is a conflict nobody resolved: staging it would
// commit the markers. Outside those states a line like "=======" is ordinary
// text and is not looked at. The error gives a count, as the paths may match
// the denylist.
func refuseConflictMarkers(repo *gitwt.Repo) error {
	if !repo.RebaseInProgress() && !repo.MergeInProgress() && !repo.HasUnmerged() {
		return nil
	}
	dirty, err := repo.DirtyPaths("")
	if err != nil {
		return err
	}
	n := 0
	for _, p := range dirty {
		b, err := os.ReadFile(filepath.Join(repo.Dir, filepath.FromSlash(p)))
		if err == nil && conflictMarkers.Match(b) {
			n++
		}
	}
	if n > 0 {
		return fmt.Errorf("refusing to stage %d file(s) with conflict markers while a merge or rebase is unresolved", n)
	}
	return nil
}
