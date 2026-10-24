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

// recordHead writes HEAD as the last propagated commit, atomically.
func recordHead(repo *gitwt.Repo) error {
	head := headRev(repo)
	if head == "" {
		return nil
	}
	return writeSynced(syncedHeadPath(repo), []byte(head+"\n"))
}

// catchUp propagates the commits that moved HEAD since the record into the
// Claude tree, then advances the record.
//
//   - No record (the first run after an upgrade): the record is set to HEAD and
//     nothing else happens.
//   - HEAD descends from the record: propagateChanges(record, HEAD), as for a
//     pull. A path that is dirty in the work-tree (held back, parked) is the
//     user's version and is skipped.
//   - HEAD does not descend from it (history was rewritten, by a rebase done by
//     hand): the diff between the two is propagated anyway, but only for a file
//     whose Claude copy still equals the record's version; for any other the
//     incoming version is kept as .from-remote-* in the mirror.
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
	dirty, err := repo.DirtyPaths("")
	if err != nil {
		return err
	}
	skip := map[string]bool{}
	for _, p := range dirty {
		skip[p] = true
	}
	g := newPathGuard(l.Cfg)
	descends, err := repo.IsAncestor(rec, head)
	if err != nil {
		return err
	}
	if descends {
		err = propagate(repo, roots, rec, head, skip, g)
	} else {
		var kept int
		kept, err = propagateRewritten(repo, roots, rec, head, skip, g)
		// Count only: a path may itself match the denylist.
		log.Printf("sync repo history was rewritten outside the daemon; propagated its changes to files you have not changed since (%d kept as .from-remote-*)", kept)
	}
	if err != nil {
		return err
	}
	return recordHead(repo)
}

// propagateRewritten is propagateChanges for a HEAD that does not descend from
// rec. A path is copied to (or deleted from) the Claude tree only if the Claude
// copy still equals the version at rec; otherwise the version at head is kept
// beside the mirror file as .from-remote-*, and the Claude copy stands. It
// returns how many were kept.
func propagateRewritten(repo *gitwt.Repo, roots Roots, rec, head string, skip map[string]bool, g *pathGuard) (int, error) {
	out, err := repo.Run("diff", "--name-status", "-z", "--no-renames", rec, head)
	if err != nil {
		return 0, err
	}
	kept := 0
	fields := strings.Split(out, "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		status, p := fields[i], fields[i+1]
		hash, name, ok := splitMirrorPath(p)
		if !ok || skip[p] || status == "" {
			continue
		}
		cp := filepath.Join(roots.Claude, hash, "memory", name)
		recBytes, recOK := blobAt(repo, rec, p)
		curOK, cur := readIfExists(cp)
		unchanged := curOK == recOK && bytes.Equal(cur, recBytes)
		switch {
		case status[0] == 'D':
			if unchanged && curOK {
				_ = os.Remove(cp)
			}
		case unchanged:
			if err := CopyToClaude(roots, hash, name); err != nil {
				log.Printf("propagate %s: %v", g.Path(p), g.Err(err))
			}
		default:
			incoming, _ := blobAt(repo, head, p)
			backup := fmt.Sprintf("%s.from-remote-%s", filepath.Join(roots.Mirror, hash, "memory", name), randSuffix())
			if err := writeAtomic(backup, incoming); err != nil {
				return kept, g.PathErr("back up incoming", p, err)
			}
			kept++
		}
	}
	return kept, nil
}

// blobAt is the content of p at rev; false if it does not exist there.
func blobAt(repo *gitwt.Repo, rev, p string) ([]byte, bool) {
	out, err := repo.Run("show", rev+":"+p)
	if err != nil {
		return nil, false
	}
	return []byte(out), true
}
