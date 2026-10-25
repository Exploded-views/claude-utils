package sync

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
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
		plan, ok := planArchiveResolutionWith(entries, modifyDeleteResolver(repo))
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
	// An encrypted repo stores ciphertext: work on the plaintext, which the
	// clean filter encrypts again on the add below. The name's hash is of the
	// plaintext.
	if plain, perr := agecrypt.PlainReader(repo)([]byte(content)); perr != nil {
		return fmt.Errorf("cannot read a conflicted blob: %w", perr)
	} else {
		content = string(plain)
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
	return planArchiveResolutionWith(entries, nil)
}

// planArchiveResolutionWith is planArchiveResolution plus a resolver for the one
// shape that needs the file contents: a live note modified on one side and deleted
// (archived) on the other, stages {1,2} or {1,3}. nil leaves it unsafe.
func planArchiveResolutionWith(entries []gitwt.UnmergedEntry, modDel func(p, base, other string) (resolution, bool)) ([]resolution, bool) {
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
			if _, _, ok := splitMirrorPath(p); ok && modDel != nil && len(st) == 2 {
				base, has1 := st[1]
				other, has2 := st[2] // modified upstream, archived by the commit
				if !has2 {
					other, has2 = st[3] // archived upstream, modified by the commit
				}
				if has1 && has2 {
					if r, ok := modDel(p, base, other); ok {
						plan = append(plan, r)
						continue
					}
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

// modifyDeleteResolver decides a live note that one side changed while the other
// archived it. It applies only when the archiving side really holds an archive copy of the base text, so a plain deletion is never taken
// for an archive. If the changed side's text is the base text (an encrypted repo
// re-encrypts every file when a recipient joins, which looks like a change
// of the ciphertext), the deletion stands. If the text was edited, the edit
// stays at the note's live path and the archive copy keeps the original: both
// versions survive, as in repairEditedArchives.
func modifyDeleteResolver(repo *gitwt.Repo) func(p, base, other string) (resolution, bool) {
	plainOf := agecrypt.PlainReader(repo)
	read := func(oid string) ([]byte, bool) {
		raw, err := repo.Run("cat-file", "blob", oid)
		if err != nil {
			return nil, false
		}
		b, err := plainOf([]byte(raw))
		return b, err == nil
	}
	return func(p, baseOid, otherOid string) (resolution, bool) {
		folder, name, ok := splitMirrorPath(p)
		if !ok {
			return resolution{}, false
		}
		base, ok1 := read(baseOid)
		up, ok2 := read(otherOid)
		if !ok1 || !ok2 {
			return resolution{}, false
		}
		archived, ok := replayedArchiveCopy(repo, folder, name, fullHash(base))
		if !ok {
			return resolution{}, false
		}
		if string(base) == string(up) {
			return resolution{path: p}, true
		}
		parts := strings.Split(archived, "/")
		if err := archive.AppendIndex(repo.Dir, archive.Entry{
			Date: parts[1], Folder: folder, File: name, Reason: reasonRestored, Path: archived,
		}); err != nil {
			return resolution{}, false
		}
		if _, err := repo.Run("add", "--", gitwt.Literal(path.Join(archive.Dir, archive.IndexName))); err != nil {
			return resolution{}, false
		}
		return resolution{p, otherOid}, true
	}
}

// replayedArchiveCopy finds, in the commit the rebase is replaying or in
// upstream, the archive copy of folder/name whose name carries the hash prefix
// of the given text.
func replayedArchiveCopy(repo *gitwt.Repo, folder, name, hash string) (string, bool) {
	for _, rev := range []string{"REBASE_HEAD", "HEAD"} {
		out, err := repo.Run("ls-tree", "-r", "--name-only", "-z", rev, "--", gitwt.Literal(archive.Dir))
		if err != nil {
			continue
		}
		for _, p := range strings.Split(out, "\x00") {
			parts := strings.Split(p, "/")
			if len(parts) != 4 || parts[2] != folder {
				continue
			}
			if orig, prefix, ok := archiveNameHash(parts[3]); ok && orig == name && strings.HasPrefix(hash, prefix) {
				return p, true
			}
		}
	}
	return "", false
}
