package sync

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/archive"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// Two hosts that archive the same note leave git a conflict that is no
// conflict: each side moved the note into archive/ under its own hash-named
// path, and git reads that as a rename on both sides, or a rename against a
// delete. Whatever the merge strategy, the right result is plain: both archive
// copies stay and the note is gone. So when the daemon's own pull stops with
// nothing but conflicts of exactly these shapes, they are resolved and the
// rebase continues. Any other conflict (or one extra path) leaves the stopped
// rebase to be aborted as before. The shapes, as ort leaves them:
//
//	archive/<date>/<folder>/<file>   stages {3} or {1,3}  the commit's copy; upstream lacks it
//	archive/<date>/<folder>/<file>   stage  {2}           upstream's copy (the other side of a rename/rename)
//	projects/<folder>/memory/<file>  stage  {1} only      deleted on both sides
//
// archive/INDEX.md is merged as a union and is never in the safe set.

// maxResolveRounds bounds the commits one pull may resolve: each local commit
// that conflicts is a round.
const maxResolveRounds = 100

// resolveArchiveConflicts resolves a rebase stopped by the daemon's pull, round
// after round, while every conflicted path is of a safe shape. It reports how
// many paths it resolved. When it returns with the rebase still stopped, that
// is a conflict it does not own and the caller aborts it.
func resolveArchiveConflicts(repo *gitwt.Repo) (int, error) {
	total := 0
	for round := 0; round < maxResolveRounds; round++ {
		if !repo.RebaseInProgress() || !stoppedByMarkedPull(repo) {
			return total, nil
		}
		entries, err := repo.UnmergedEntries()
		if err != nil {
			return total, err
		}
		plan, ok := planArchiveResolution(entries)
		if !ok {
			return total, nil
		}
		for _, p := range plan {
			if err := p.apply(repo); err != nil {
				return total, err
			}
		}
		total += len(plan)
		var env []string
		if b, err := os.ReadFile(pullMarkerPath(repo)); err == nil {
			env = append(env, "GIT_REFLOG_ACTION="+pullAction(strings.TrimSpace(string(b))))
		}
		if err := repo.RebaseContinue(env...); err != nil && repo.RebaseInProgress() {
			// Stopped again (the next local commit). Loop; an unsafe stop ends it.
			if left, _ := repo.UnmergedPaths(); len(left) == 0 {
				return total, err // stopped for another reason
			}
		}
	}
	return total, nil
}

type resolution struct {
	path string
	oid  string // blob to put at path; "" means remove it
}

func (r resolution) apply(repo *gitwt.Repo) error {
	if r.oid == "" {
		_, err := repo.Run("rm", "-q", "--cached", "--", gitwt.Literal(r.path))
		if err == nil {
			err = os.Remove(filepath.Join(repo.Dir, filepath.FromSlash(r.path)))
			if os.IsNotExist(err) {
				err = nil
			}
		}
		return err
	}
	content, err := repo.Run("cat-file", "blob", r.oid)
	if err != nil {
		return err
	}
	// A rename/rename merges one side's edit into the other side's copy. A copy
	// must hold the bytes it is named for: put those back (they are in history).
	if parts := strings.Split(r.path, "/"); len(parts) == 4 && parts[2] != archive.DistilledFolder {
		if orig, prefix, ok := archiveNameHash(parts[3]); ok && !archiveBytesMatch(parts[3], []byte(content)) {
			b, found := findOriginal(repo, "HEAD", r.path, parts[2], orig, prefix)
			if !found {
				return fmt.Errorf("an archive copy does not match its name and its original text cannot be found")
			}
			content = string(b)
		}
	}
	full := filepath.Join(repo.Dir, filepath.FromSlash(r.path))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		return err
	}
	_, err = repo.Run("add", "--", gitwt.Literal(r.path))
	return err
}

// planArchiveResolution decides, from the conflicted index entries alone,
// whether every path is of a safe shape, and what to do with each.
func planArchiveResolution(entries []gitwt.UnmergedEntry) ([]resolution, bool) {
	if len(entries) == 0 {
		return nil, false
	}
	byPath := map[string]map[int]string{}
	var order []string
	for _, e := range entries {
		if byPath[e.Path] == nil {
			byPath[e.Path] = map[int]string{}
			order = append(order, e.Path)
		}
		byPath[e.Path][e.Stage] = e.Oid
	}
	var plan []resolution
	for _, p := range order {
		st := byPath[p]
		switch {
		case strings.HasPrefix(p, archive.Dir+"/") && p != path.Join(archive.Dir, archive.IndexName):
			two, three := st[2], st[3]
			_, has2 := st[2]
			_, has3 := st[3]
			switch {
			case has2 && has3 && two == three:
				plan = append(plan, resolution{p, two})
			case has3 && !has2:
				plan = append(plan, resolution{p, three}) // {3}, {1,3}
			case has2 && !has3 && len(st) == 1:
				plan = append(plan, resolution{p, two}) // {2}
			default:
				return nil, false // {1,2}: the commit deleted a copy; {2,3} differing
			}
		default:
			if _, _, ok := splitMirrorPath(p); ok && len(st) == 1 {
				if _, only1 := st[1]; only1 {
					plan = append(plan, resolution{path: p})
					continue
				}
			}
			return nil, false
		}
	}
	return plan, true
}

// resolvedLine is the cycle's one log line about it: a count, no paths.
func resolvedLine(n int) string {
	return fmt.Sprintf("pull: auto-resolved %d archive conflict(s) (both archive copies kept)", n)
}
