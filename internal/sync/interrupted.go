package sync

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// pullMarkerFile is written to .state before the daemon's own pull and removed
// after it. A rebase or merge found stopped while it exists is the daemon's
// (a crash in the pull left it); one found without it was started by the user,
// who is resolving it by hand.
const pullMarkerFile = "pull-running"

func pullMarkerPath(repo *gitwt.Repo) string {
	return filepath.Join(repo.Dir, manifestStateDir, pullMarkerFile)
}

func markPull(repo *gitwt.Repo) error { return writeSynced(pullMarkerPath(repo), nil) }

func pullMarked(repo *gitwt.Repo) bool {
	_, err := os.Stat(pullMarkerPath(repo))
	return err == nil
}

// clearPullMark removes the marker, unless a rebase or merge is still stopped:
// that one is still the daemon's to abort.
func clearPullMark(repo *gitwt.Repo) {
	if repo.RebaseInProgress() || repo.MergeInProgress() {
		return
	}
	_ = os.Remove(pullMarkerPath(repo))
}

// errHandResolving ends a cycle early: a rebase or merge is stopped in the sync
// repo and the daemon did not start it.
var errHandResolving = errors.New("a rebase or merge is in progress in the sync repo and the daemon did not start it; assuming you are resolving it by hand, this cycle stages, reconciles, pulls and pushes nothing until it is finished")

// abortDaemonInterrupted is abortInterrupted for what the daemon's own pull
// left. A stopped rebase or merge without the marker is the user's, and is left
// alone: it returns errHandResolving.
func abortDaemonInterrupted(repo *gitwt.Repo) error {
	if (repo.RebaseInProgress() || repo.MergeInProgress()) && !pullMarked(repo) {
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
