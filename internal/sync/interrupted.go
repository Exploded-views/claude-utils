package sync

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// abortInterrupted abandons a rebase or merge that was left stopped, which is
// what a crash (or a kill) in the middle of a pull leaves behind. It runs at
// startup and at the start of every flush, whether or not anything is parked:
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
