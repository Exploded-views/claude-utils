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
	// A crash before the state file was written leaves bytes nobody refers to.
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
		return nil, fmt.Errorf("parse %s: %w", parkedJSON, err)
	}
	for _, e := range st.Paths {
		// The file is ours, but a path that escapes the work-tree would make
		// the restore write outside it.
		if e.Path == "" || path.IsAbs(e.Path) || path.Clean(e.Path) != e.Path || e.Path == ".." || strings.HasPrefix(e.Path, "../") {
			return nil, fmt.Errorf("%s holds an invalid path", parkedJSON)
		}
		if e.Kind != parkFile && e.Kind != parkDeleted {
			return nil, fmt.Errorf("%s holds an unknown kind %q", parkedJSON, e.Kind)
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
// first (keepIncoming). Then the parked version is restored: for a memory
// file, by refilling the mirror from the Claude copy, which is authoritative;
// for any other path, from the saved bytes.
func finishParked(repo *gitwt.Repo, roots Roots) error {
	st, err := loadPark(repo)
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
		st.Paths = left
		if err := writeParkState(repo, st); err != nil {
			errs = append(errs, err)
		}
		return errors.Join(errs...)
	}
	// The state file goes first: if the saved bytes outlive it, the next park
	// just clears them, while a state file without its bytes could never finish.
	if err := os.Remove(parkStatePath(repo)); err != nil {
		return err
	}
	return os.RemoveAll(parkBytesDir(repo))
}

// restoreParked puts one parked path back. It reports whether a
// .from-remote-* backup of an incoming version was written.
func restoreParked(repo *gitwt.Repo, roots Roots, preRev string, e parkedPath, changedUpstream bool) (bool, error) {
	var saved []byte
	if e.Kind == parkFile {
		b, err := os.ReadFile(filepath.Join(parkBytesDir(repo), filepath.FromSlash(e.Path)))
		if err != nil {
			return false, err
		}
		saved = b
	}
	hash, name, isMemory := splitMirrorPath(e.Path)
	kept := false
	if changedUpstream {
		var err error
		if kept, err = keepIncoming(repo, roots, preRev, e.Path, saved, e.Kind == parkFile); err != nil {
			return kept, err
		}
	}
	if isMemory {
		err := CopyToMirror(roots, hash, name)
		if errors.Is(err, fs.ErrNotExist) {
			err = RemoveFromMirror(roots, hash, name)
		}
		return kept, err
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
	return kept, writeAtomic(dst, saved)
}

// keepIncoming preserves the pulled version of parked path p (it is in the
// work-tree now). It returns true when a backup file was written.
//
// The user's version of a memory file is its Claude copy; of any other path,
// the parked bytes. A MEMORY.md is merged 3-way with the incoming version
// (against the version at preRev) and the result written to the Claude copy.
// Anything else, or a MEMORY.md whose merge conflicts, keeps the incoming
// version beside the work-tree file as .from-remote-*, as Reconcile does.
func keepIncoming(repo *gitwt.Repo, roots Roots, preRev, p string, saved []byte, existed bool) (bool, error) {
	wp := filepath.Join(repo.Dir, filepath.FromSlash(p))
	incoming, err := os.ReadFile(wp)
	if err != nil {
		return false, nil // deleted upstream: the user's version stands
	}
	hash, name, isMemory := splitMirrorPath(p)
	local, present := saved, existed
	if isMemory {
		cp := filepath.Join(roots.Claude, hash, "memory", name)
		b, err := os.ReadFile(cp)
		if err != nil {
			return false, nil // no Claude copy: the user's version is its absence
		}
		local, present = b, true
		if name == "MEMORY.md" && !bytes.Equal(local, incoming) {
			base, _ := repo.Run("show", preRev+":"+p) // "" when it did not exist
			if merged, conflict := merge.Merge(base, string(local), string(incoming)); !conflict {
				return false, writeAtomic(cp, []byte(merged))
			}
		}
	}
	if present && bytes.Equal(local, incoming) {
		return false, nil
	}
	return true, writeAtomic(fmt.Sprintf("%s.from-remote-%s", wp, randSuffix()), incoming)
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
