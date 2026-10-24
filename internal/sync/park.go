package sync

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/merge"
)

// Parking sets the uncommitted files of the sync work-tree aside around the
// pull, so `pull --rebase` runs on a clean tree.
//
// Whatever CommitMirror left uncommitted (files held back by the scrub, or
// everything when the denylist policy blocks the commit) is dirty. A dirty
// tree makes git autostash, and autostash conflicts with an incoming change to
// the same file, and refuses outright to overwrite an untracked file. So,
// before the pull, every dirty path in the work-tree (projects/, distilled/,
// anything else) is copied to .state/parked/<path> and listed in
// .state/parked.json, and the tree is then reset to HEAD. After the pull each
// path is put back (finishParked).
//
// The order makes a crash at any point recoverable: the bytes are saved and
// the state file is written (and fsynced) before the work-tree is touched, and
// the state is deleted only after every path is restored. finishParked is also
// run when the daemon starts, before Reconcile. A rebase or merge left stopped is
// aborted first (see abortInterrupted), parked or not.

const (
	parkFile    = "file"    // the path existed; its bytes are in .state/parked/<path>
	parkDeleted = "deleted" // a tracked path the user had deleted

	parkedJSON = "parked.json"
	parkedDir  = "parked"
)

type parkedPath struct {
	Path string `json:"path"` // slash-separated, relative to the work-tree root
	Kind string `json:"kind"`
}

type parkState struct {
	PreRev string       `json:"preRev"` // HEAD when parking began; "" if unborn
	Paths  []parkedPath `json:"paths"`
	// Failures counts, per path, the consecutive restore attempts that failed.
	Failures map[string]int `json:"failures,omitempty"`
}

// skip returns the parked paths as a set, for propagateChanges.
func (s *parkState) skip() map[string]bool {
	set := map[string]bool{}
	if s != nil {
		for _, p := range s.Paths {
			set[p.Path] = true
		}
	}
	return set
}

func parkStatePath(repo *gitwt.Repo) string {
	return filepath.Join(repo.Dir, manifestStateDir, parkedJSON)
}

func parkBytesDir(repo *gitwt.Repo) string {
	return filepath.Join(repo.Dir, manifestStateDir, parkedDir)
}

// parkUncommitted saves every dirty path, records it, and resets the tree to
// HEAD. It returns nil when nothing is dirty. On error the state may be on
// disk; finishParked puts everything back.
func parkUncommitted(repo *gitwt.Repo) (*parkState, error) {
	st, err := savePark(repo)
	if err != nil || st == nil {
		return st, err
	}
	return st, resetParked(repo, st)
}

// savePark copies each dirty path's bytes under .state/parked/ and then writes
// parked.json, atomically and fsynced. It does not touch the work-tree.
func savePark(repo *gitwt.Repo) (*parkState, error) {
	dirty, err := repo.DirtyPaths("")
	if err != nil {
		return nil, err
	}
	st := &parkState{}
	// Bytes with no state file beside them are nobody's, or the only copy of a
	// set-aside that failed halfway: never deleted, moved aside.
	if err := setAsideOrphanBytes(repo); err != nil {
		return nil, err
	}
	if err := os.RemoveAll(parkBytesDir(repo)); err != nil {
		return nil, err
	}
	for _, p := range dirty {
		// The denylist is per-PC config that is read from here: never move it.
		if strings.EqualFold(p, denylistRel) {
			continue
		}
		info, err := os.Lstat(filepath.Join(repo.Dir, filepath.FromSlash(p)))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			st.Paths = append(st.Paths, parkedPath{Path: p, Kind: parkDeleted})
		case err != nil:
			return nil, err
		case !info.Mode().IsRegular():
			continue // a symlink or the like: left for git to carry as it always did
		default:
			b, err := os.ReadFile(filepath.Join(repo.Dir, filepath.FromSlash(p)))
			if err != nil {
				return nil, err
			}
			if err := writeSynced(filepath.Join(parkBytesDir(repo), filepath.FromSlash(p)), b); err != nil {
				return nil, err
			}
			st.Paths = append(st.Paths, parkedPath{Path: p, Kind: parkFile})
		}
	}
	if len(st.Paths) == 0 {
		return nil, nil
	}
	rev, _ := repo.Run("rev-parse", "HEAD") // fails on an unborn branch: ""
	st.PreRev = strings.TrimSpace(rev)
	if err := writeParkState(repo, st); err != nil {
		return nil, err
	}
	return st, nil
}

// setAsideOrphanBytes moves a parked/ directory that has no parked.json beside
// it into a new .state/parked.broken-<ts>/, and logs where.
func setAsideOrphanBytes(repo *gitwt.Repo) error {
	if _, err := os.Stat(parkBytesDir(repo)); err != nil || parkedExists(repo) {
		return nil
	}
	dir, err := newBrokenDir(repo)
	if err != nil {
		return err
	}
	if err := moveFile(parkBytesDir(repo), filepath.Join(dir, parkedDir)); err != nil {
		return err
	}
	log.Printf("%s/ had no %s beside it; moved to %s so a new park cannot delete it", parkedDir, parkedJSON, filepath.ToSlash(filepath.Join(manifestStateDir, filepath.Base(dir))))
	return nil
}

func writeParkState(repo *gitwt.Repo, st *parkState) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeSynced(parkStatePath(repo), b)
}

// resetParked returns each parked path to HEAD, removing it if HEAD lacks it.
func resetParked(repo *gitwt.Repo, st *parkState) error {
	inHead, err := repo.HeadFiles("")
	if err != nil {
		return err
	}
	// Nothing parked may stay staged either.
	if err := repo.UnstageAll(); err != nil {
		return err
	}
	var restore []string
	for _, e := range st.Paths {
		if inHead[e.Path] {
			restore = append(restore, e.Path)
		} else if err := os.Remove(filepath.Join(repo.Dir, filepath.FromSlash(e.Path))); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return repo.CheckoutHead(restore)
}

// errBrokenPark marks a parked.json that cannot be used: it does not parse, or
// it fails validation. Reading it at all (permissions, say) is a different
// error.
var errBrokenPark = errors.New("unusable parked state")

// maxRestoreFailures is how many consecutive failed restores of one path are
// tolerated before its bytes are set aside.
const maxRestoreFailures = 3

// parkedExists reports whether a parked.json is on disk, i.e. a park is
// unfinished.
func parkedExists(repo *gitwt.Repo) bool {
	_, err := os.Stat(parkStatePath(repo))
	return err == nil
}

// loadPark reads parked.json; (nil, nil) when there is none.
func loadPark(repo *gitwt.Repo) (*parkState, error) {
	b, err := os.ReadFile(parkStatePath(repo))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st parkState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("%w: parse %s: %v", errBrokenPark, parkedJSON, err)
	}
	for _, e := range st.Paths {
		// The file is ours, but a path that escapes the work-tree would make
		// the restore write outside it.
		if e.Path == "" || path.IsAbs(e.Path) || path.Clean(e.Path) != e.Path || e.Path == ".." || strings.HasPrefix(e.Path, "../") {
			return nil, fmt.Errorf("%w: %s holds an invalid path", errBrokenPark, parkedJSON)
		}
		if e.Kind != parkFile && e.Kind != parkDeleted {
			return nil, fmt.Errorf("%w: %s holds an unknown kind %q", errBrokenPark, parkedJSON, e.Kind)
		}
	}
	return &st, nil
}

// finishParked puts every parked path back and deletes the state. It does
// nothing when no state is on disk. It tries every path: those it restores are
// dropped from the state, and the rest stay recorded for the next try, with
// the error returned.
//
// For a path the pull changed (preRev..HEAD), the incoming version is kept
// first (keepIncoming). Then the parked version is restored from the saved
// bytes, for every path alike.
func finishParked(repo *gitwt.Repo, roots Roots) error {
	st, err := loadPark(repo)
	if errors.Is(err, errBrokenPark) {
		// Waiting on a state file that can never be read would halt sync for
		// good: set it aside with its bytes, say so, and carry on.
		dir, qerr := quarantinePark(repo)
		if qerr != nil {
			return errors.Join(err, qerr)
		}
		log.Printf("PARKED STATE UNUSABLE (%v): parked.json and parked/ moved to %s; held-back files from that cycle are not restored from it, see the docs to recover them", err, dir)
		return nil
	}
	if err != nil || st == nil {
		return err
	}
	upstream := map[string]bool{}
	postRev, _ := repo.Run("rev-parse", "HEAD")
	postRev = strings.TrimSpace(postRev)
	if st.PreRev != "" && postRev != "" && st.PreRev != postRev {
		out, err := repo.Run("diff", "--name-only", "-z", "--no-renames", st.PreRev, postRev)
		if err != nil {
			return err // without it we could not tell what the pull changed
		}
		for _, p := range strings.Split(out, "\x00") {
			upstream[p] = true
		}
	}

	var errs []error
	var left []parkedPath
	backedUp := 0
	for _, e := range st.Paths {
		kept, err := restoreParked(repo, roots, st.PreRev, e, upstream[e.Path])
		if kept {
			backedUp++
		}
		if err != nil {
			errs = append(errs, err)
			left = append(left, e)
		}
	}
	if backedUp > 0 {
		// Count only: a path may itself match the denylist.
		log.Printf("scrub: %d held-back file(s) changed upstream; incoming copies kept as .from-remote-*", backedUp)
	}
	if len(errs) > 0 {
		// A path that fails again and again would block sync for good: after
		// maxRestoreFailures cycles in a row its bytes are set aside.
		failures := map[string]int{}
		var stuck []parkedPath
		var still []parkedPath
		for _, e := range left {
			if n := st.Failures[e.Path] + 1; n >= maxRestoreFailures {
				stuck = append(stuck, e)
			} else {
				failures[e.Path] = n
				still = append(still, e)
			}
		}
		if len(stuck) > 0 {
			dir, qerr := quarantinePaths(repo, stuck)
			if qerr != nil {
				return errors.Join(append(errs, qerr)...)
			}
			refreshMirror(roots, stuck)
			log.Printf("PARKED FILES STUCK: %d path(s) could not be restored in %d cycles; their saved bytes were moved to %s, see the docs to recover them", len(stuck), maxRestoreFailures, dir)
		}
		if len(still) == 0 {
			return clearPark(repo) // all that failed was set aside: nothing blocks the cycle
		}
		st.Paths, st.Failures = still, failures
		if err := writeParkState(repo, st); err != nil {
			errs = append(errs, err)
		}
		return errors.Join(errs...)
	}
	return clearPark(repo)
}

// clearPark deletes the state, then the saved bytes. The state file goes first:
// if the bytes outlive it, the next park moves them aside (setAsideOrphanBytes),
// while a state file without its bytes could never finish.
func clearPark(repo *gitwt.Repo) error {
	if err := os.Remove(parkStatePath(repo)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.RemoveAll(parkBytesDir(repo))
}

// newBrokenDir creates .state/parked.broken-<unix time> (with a counter if
// that name is taken) and returns its path.
func newBrokenDir(repo *gitwt.Repo) (string, error) {
	state := filepath.Join(repo.Dir, manifestStateDir)
	if err := os.MkdirAll(state, 0700); err != nil {
		return "", err
	}
	base := fmt.Sprintf("parked.broken-%d", time.Now().Unix())
	for i := 1; ; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		dir := filepath.Join(state, name)
		err := os.Mkdir(dir, 0700)
		if err == nil {
			return dir, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
}

// moveFile is os.Rename; a variable so a test can make a move fail.
var moveFile = os.Rename

// quarantinePark moves a parked.json that cannot be used, and the saved bytes
// beside it, into a new .state/parked.broken-<ts>/. It returns the directory's
// name relative to the sync dir, which is safe to log.
func quarantinePark(repo *gitwt.Repo) (string, error) {
	dir, err := newBrokenDir(repo)
	if err != nil {
		return "", err
	}
	// The bytes go first: if the second move fails, parked.json is still in
	// place and the next try finishes the job. The other order could leave
	// bytes without a state file, to be cleared by the next savePark.
	for _, m := range []struct{ src, name string }{{parkBytesDir(repo), parkedDir}, {parkStatePath(repo), parkedJSON}} {
		if err := moveFile(m.src, filepath.Join(dir, m.name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	return filepath.ToSlash(filepath.Join(manifestStateDir, filepath.Base(dir))), nil
}

// quarantinePaths moves the saved bytes of paths whose restore keeps failing
// into a new .state/parked.broken-<ts>/parked/, with a parked.json listing
// them. A deleted path has no bytes; it is only listed.
func quarantinePaths(repo *gitwt.Repo, paths []parkedPath) (string, error) {
	dir, err := newBrokenDir(repo)
	if err != nil {
		return "", err
	}
	for _, e := range paths {
		if e.Kind != parkFile {
			continue
		}
		dst := filepath.Join(dir, parkedDir, filepath.FromSlash(e.Path))
		if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
			return "", err
		}
		src := filepath.Join(parkBytesDir(repo), filepath.FromSlash(e.Path))
		if err := os.Rename(src, dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	b, err := json.MarshalIndent(&parkState{Paths: paths}, "", "  ")
	if err != nil {
		return "", err
	}
	if err := writeSynced(filepath.Join(dir, parkedJSON), b); err != nil {
		return "", err
	}
	return filepath.ToSlash(filepath.Join(manifestStateDir, filepath.Base(dir))), nil
}

// refreshMirror makes the mirror copy of each set-aside memory path match its
// Claude copy, which is authoritative (removed if the Claude copy is gone). The
// mirror copy is HEAD's version, since the park reset it; left alone, a
// Reconcile would merge it back into the Claude copy, bringing back a MEMORY.md
// section the user had removed. A failure is only logged: the Claude copy is
// untouched, and the next cycle's watcher event or Reconcile syncs it anyway.
// The log gives the count only, as a path may match the denylist.
func refreshMirror(roots Roots, paths []parkedPath) {
	failed := 0
	for _, e := range paths {
		hash, name, ok := splitMirrorPath(e.Path)
		if !ok || !roots.owns(hash) {
			continue
		}
		err := CopyToMirror(roots, hash, name)
		if errors.Is(err, fs.ErrNotExist) {
			err = RemoveFromMirror(roots, hash, name)
		}
		if err != nil {
			failed++
		}
	}
	if failed > 0 {
		log.Printf("could not refresh the mirror copy of %d set-aside file(s)", failed)
	}
}

// restoreParked puts one parked path back, from its saved bytes. It reports
// whether a .from-remote-* backup of an incoming version was written.
func restoreParked(repo *gitwt.Repo, roots Roots, preRev string, e parkedPath, changedUpstream bool) (bool, error) {
	var saved []byte
	if e.Kind == parkFile {
		b, err := os.ReadFile(filepath.Join(parkBytesDir(repo), filepath.FromSlash(e.Path)))
		if err != nil {
			return false, err
		}
		saved = b
	}
	final := saved
	kept := false
	if changedUpstream {
		var err error
		if kept, final, err = keepIncoming(repo, roots, preRev, e.Path, saved, e.Kind == parkFile); err != nil {
			return kept, err
		}
	}
	dst := filepath.Join(repo.Dir, filepath.FromSlash(e.Path))
	if e.Kind == parkDeleted {
		if err := os.Remove(dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return kept, err
		}
		return kept, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return kept, err
	}
	return kept, writeAtomic(dst, final)
}

// keepIncoming preserves the pulled version of parked path p (it is in the
// work-tree now). It returns true when a backup file was written, and the bytes
// the work-tree file is to end up with: the saved bytes, or for a MEMORY.md
// that merged cleanly, the merge.
//
// The user's version of every path is the saved bytes, a memory file included:
// whatever the Claude copy holds now is for a watcher event or Reconcile to
// sync later. A MEMORY.md is merged 3-way with the incoming version (against
// the version at preRev), but only if its Claude copy still equals the saved
// bytes; then the merge is written to the Claude copy too. Anything else, a
// MEMORY.md whose merge conflicts or whose Claude copy has moved on, or a
// parked deletion, keeps the incoming version beside the work-tree file as
// .from-remote-*, as Reconcile does.
func keepIncoming(repo *gitwt.Repo, roots Roots, preRev, p string, saved []byte, existed bool) (bool, []byte, error) {
	wp := filepath.Join(repo.Dir, filepath.FromSlash(p))
	incoming, err := os.ReadFile(wp)
	if err != nil {
		return false, saved, nil // deleted upstream: the user's version stands
	}
	if existed && bytes.Equal(saved, incoming) {
		return false, saved, nil
	}
	if hash, name, ok := splitMirrorPath(p); ok && roots.owns(hash) && name == "MEMORY.md" && existed {
		cp := filepath.Join(roots.Claude, hash, "memory", name)
		if cur, err := os.ReadFile(cp); err == nil && bytes.Equal(cur, saved) {
			base, _ := repo.Run("show", preRev+":"+p) // "" when it did not exist
			if merged, conflict := merge.Merge(base, string(saved), string(incoming)); !conflict {
				return false, []byte(merged), writeAtomic(cp, []byte(merged))
			}
		}
	}
	return true, saved, writeBackup(wp, incoming)
}

// writeSynced is writeAtomic plus an fsync of the data and of the directory,
// so the file is on disk before the caller goes on to change anything else.
func writeSynced(p string, content []byte) error {
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp := filepath.Join(dir, fmt.Sprintf(".%s.%s.tmp", filepath.Base(p), randSuffix()))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(content)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, p)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if d, err := os.Open(dir); err == nil { // best effort: not possible on every OS
		_ = d.Sync()
		d.Close()
	}
	return nil
}
