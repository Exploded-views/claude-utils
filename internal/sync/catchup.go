package sync

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
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

// SyncedHead returns the commit recorded as copied into the Claude tree, or
// "" when there is no record. The record may name a commit that is gone.
func SyncedHead(repo *gitwt.Repo) (string, error) { return loadSyncedHead(repo) }

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

// detachedLogged is set once the detached HEAD is logged, so a HEAD that stays
// detached is logged once, not every cycle.
var detachedLogged atomic.Bool

// recordHead writes HEAD as the last propagated commit, atomically. It writes
// nothing while a rebase or merge is stopped or HEAD is detached: HEAD is then
// somewhere in the middle of the user's resolution, and recording it would make
// the commits below it look propagated when they were never copied to ~/.claude.
func recordHead(repo *gitwt.Repo) error {
	head := headRev(repo)
	if head == "" || handResolving(repo) {
		return nil
	}
	if repo.HeadDetached() {
		if !detachedLogged.Swap(true) {
			log.Println("HEAD of the sync repo is detached: not recording what was synced until it is on a branch again (git -C ~/.claudesync checkout main)")
		}
		return nil
	}
	detachedLogged.Store(false)
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
//     parked) is skipped without a backup: autostash already merged it with
//     the incoming change, or the park did. A path whose Claude copy no longer
//     equals the record's version is skipped too: it holds a change of yours
//     that was never committed. The incoming version of that one is kept as
//     .from-remote-* in the mirror, written once the propagate and the record
//     write have succeeded (so a catch-up that fails is retried without
//     leaving another backup), and not at all if the same bytes are already
//     kept there. A Claude copy that already equals the version at HEAD (a
//     crash before the record write) is not a change of yours.
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
	skip, keep, err := catchUpSkips(repo, roots, rec, head)
	if err != nil {
		return err
	}
	g := newPathGuard(l.Cfg)
	if err := propagate(repo, roots, rec, head, skip, g); err != nil {
		return err
	}
	if err := recordHead(repo); err != nil {
		return err
	}
	for _, k := range keep {
		if err := writeBackup(k.mirror, k.incoming); err != nil {
			return g.PathErr("write backup", k.rel, err)
		}
	}
	if len(keep) > 0 {
		// Count only: a path may itself match the denylist.
		log.Printf("catch-up: %d file(s) changed in ~/.claude since the last sync; incoming copies kept as .from-remote-*", len(keep))
	}
	return nil
}

// keptCopy is an incoming version that catchUp keeps beside the mirror file.
type keptCopy struct {
	rel      string // sync-repo path
	mirror   string // the mirror file; the backup is named after it
	incoming []byte
}

// catchUpSkips is the set of paths in rec..head that catchUp must not copy
// into the Claude tree: those dirty in the work-tree, and those whose Claude
// copy differs from the version at rec (and from the one at head). For the
// latter it also returns the version at head, to be written beside the mirror
// file as .from-remote-* (nothing, for a deletion).
func catchUpSkips(repo *gitwt.Repo, roots Roots, rec, head string) (map[string]bool, []keptCopy, error) {
	skip := map[string]bool{}
	dirty, err := repo.DirtyPaths("")
	if err != nil {
		return nil, nil, err
	}
	for _, p := range dirty {
		skip[p] = true
	}
	out, err := repo.Run("diff", "--name-status", "-z", "--no-renames", rec, head)
	if err != nil {
		return nil, nil, err
	}
	var keep []keptCopy
	fields := strings.Split(out, "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		status, p := fields[i], fields[i+1]
		hash, name, ok := splitMirrorPath(p)
		if !ok || skip[p] || status == "" || !roots.owns(hash) {
			continue
		}
		if status[0] != 'D' && undecryptableAt(repo, head, p) {
			skip[p] = true // never copy ciphertext; a later re-encryption brings the file again
			continue
		}
		curOK, cur := readIfExists(filepath.Join(roots.Claude, hash, "memory", name))
		recBytes, recOK := blobAt(repo, rec, p)
		if curOK == recOK && bytes.Equal(cur, recBytes) {
			continue
		}
		incoming, headOK := blobAt(repo, head, p)
		if curOK == headOK && bytes.Equal(cur, incoming) {
			continue // already there: the record write was missed
		}
		skip[p] = true
		if status[0] == 'D' {
			continue
		}
		keep = append(keep, keptCopy{p, filepath.Join(roots.Mirror, hash, "memory", name), incoming})
	}
	return skip, keep, nil
}

// undecryptableAt reports whether p at rev is ciphertext this host cannot read.
func undecryptableAt(repo *gitwt.Repo, rev, p string) bool {
	out, err := repo.Run("show", rev+":"+p)
	if err != nil || !agecrypt.IsCiphertext([]byte(out)) {
		return false
	}
	_, err = agecrypt.PlainReader(repo)([]byte(out))
	return err != nil
}

// blobAt is the content of p at rev; false if it does not exist there.
func blobAt(repo *gitwt.Repo, rev, p string) ([]byte, bool) {
	out, err := repo.Run("show", rev+":"+p)
	if err != nil {
		return nil, false
	}
	// An encrypted repo stores ciphertext; the Claude tree gets plaintext.
	plain, err := agecrypt.PlainReader(repo)([]byte(out))
	if err != nil {
		return nil, false
	}
	return plain, true
}
