package sync

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// syncedHeadFile records, in .state, the last commit of the sync repo whose
// files are reflected in the Claude tree: HEAD at the end of the last finished
// cycle. A `git pull` run by hand (the documented conflict resolution, say)
// moves HEAD outside a cycle. The daemon's next pull is then a no-op, so
// propagateChanges would never see those files, and Reconcile would let the
// stale Claude copies win and push them over the peer's edits. catchUp compares
// HEAD with the record before anything else and propagates the difference.
const syncedHeadFile = "synced-head"

func syncedHeadPath(repo *gitwt.Repo) string {
	return filepath.Join(repo.Dir, manifestStateDir, syncedHeadFile)
}

// headRev is HEAD's commit id; "" on an unborn branch.
func headRev(repo *gitwt.Repo) string {
	rev, err := repo.Run("rev-parse", "--verify", "--quiet", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(rev)
}

// loadSyncedHead reads the record; "" when there is none.
func loadSyncedHead(repo *gitwt.Repo) (string, error) {
	b, err := os.ReadFile(syncedHeadPath(repo))
	if os.IsNotExist(err) {
		return "", nil
	}
	return strings.TrimSpace(string(b)), err
}

// recordHead writes HEAD as the last propagated commit, atomically. It writes
// nothing while a rebase or merge is stopped or HEAD is detached: HEAD is then
// somewhere in the middle of the user's resolution, and recording it would make
// the commits below it look propagated when they were never copied to ~/.claude.
func recordHead(repo *gitwt.Repo) error {
	head := headRev(repo)
	if head == "" || handResolving(repo) || repo.HeadDetached() {
		return nil
	}
	return writeSynced(syncedHeadPath(repo), []byte(head+"\n"))
}

// catchUp propagates the commits that moved HEAD since the record into the
// Claude tree, then advances the record.
//
//   - No record (the first run after an upgrade): the record is set to HEAD and
//     nothing else happens.
//   - Otherwise propagateChanges(record, HEAD) runs as for a pull, whether HEAD
//     descends from the record or not (history rewritten by a rebase done by
//     hand; that is logged). A path that is dirty in the work-tree (held back,
//     parked) is skipped, and so is one whose Claude copy no longer equals the
//     record's version: it holds a change of yours that was never committed.
//     The incoming version of that one is kept as .from-remote-* in the mirror.
func (l *Loop) catchUp(repo *gitwt.Repo, roots Roots) error {
	head := headRev(repo)
	if head == "" {
		return nil
	}
	rec, err := loadSyncedHead(repo)
	if err != nil {
		return err
	}
	if rec == head {
		return nil
	}
	if rec == "" {
		return recordHead(repo)
	}
	if _, err := repo.Run("cat-file", "-e", rec+"^{commit}"); err != nil {
		log.Println("synced head record names an unknown commit; starting over from HEAD")
		return recordHead(repo)
	}
	descends, err := repo.IsAncestor(rec, head)
	if err != nil {
		return err
	}
	if !descends {
		log.Println("sync repo history was rewritten outside the daemon; propagating the difference for files you have not changed since")
	}
	skip, kept, err := catchUpSkips(repo, roots, rec, head)
	if err != nil {
		return err
	}
	if kept > 0 {
		// Count only: a path may itself match the denylist.
		log.Printf("catch-up: %d file(s) changed in ~/.claude since the last sync; incoming copies kept as .from-remote-*", kept)
	}
	if err := propagate(repo, roots, rec, head, skip, newPathGuard(l.Cfg)); err != nil {
		return err
	}
	return recordHead(repo)
}

// catchUpSkips is the set of paths in rec..head that catchUp must not copy
// into the Claude tree: those dirty in the work-tree, and those whose Claude
// copy differs from the version at rec. For the latter the version at head is
// written beside the mirror file as .from-remote-* (nothing, for a deletion).
// It returns how many were kept.
func catchUpSkips(repo *gitwt.Repo, roots Roots, rec, head string) (map[string]bool, int, error) {
	skip := map[string]bool{}
	dirty, err := repo.DirtyPaths("")
	if err != nil {
		return nil, 0, err
	}
	for _, p := range dirty {
		skip[p] = true
	}
	out, err := repo.Run("diff", "--name-status", "-z", "--no-renames", rec, head)
	if err != nil {
		return nil, 0, err
	}
	kept := 0
	fields := strings.Split(out, "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		status, p := fields[i], fields[i+1]
		hash, name, ok := splitMirrorPath(p)
		if !ok || skip[p] || status == "" {
			continue
		}
		recBytes, recOK := blobAt(repo, rec, p)
		curOK, cur := readIfExists(filepath.Join(roots.Claude, hash, "memory", name))
		if curOK == recOK && bytes.Equal(cur, recBytes) {
			continue
		}
		skip[p] = true
		if status[0] == 'D' {
			continue
		}
		incoming, _ := blobAt(repo, head, p)
		backup := fmt.Sprintf("%s.from-remote-%s", filepath.Join(roots.Mirror, hash, "memory", name), randSuffix())
		if err := writeAtomic(backup, incoming); err != nil {
			return nil, kept, err
		}
		kept++
	}
	return skip, kept, nil
}

// blobAt is the content of p at rev; false if it does not exist there.
func blobAt(repo *gitwt.Repo, rev, p string) ([]byte, bool) {
	out, err := repo.Run("show", rev+":"+p)
	if err != nil {
		return nil, false
	}
	return []byte(out), true
}
