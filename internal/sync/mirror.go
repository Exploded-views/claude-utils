// Package sync contains the two-way file mirror between Claude's authoritative
// memory tree (~/.claude/projects/<hash>/memory/) and the sync repo's
// work-tree (~/.claudesync/projects/<hash>/memory/), plus the daemon loop
// that keeps them in sync via git.
package sync

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/merge"
)

// Roots names a pair of (Claude authoritative, mirror) parents.
//
// For example:
//
//	Claude: C:\Users\rocky\.claude\projects
//	Mirror: C:\Users\rocky\.claudesync\projects
//
// Each child directory under either root represents one project (named by
// Claude's hash of the absolute project path).
type Roots struct {
	Claude string // ~/.claude/projects
	Mirror string // ~/.claudesync/projects

	// Owns, if set, says whether this repo may touch a Claude folder. A folder
	// it rejects is invisible to the repo: Reconcile, the watcher, propagate,
	// catch-up, park restores and the manifest all skip it. nil means every
	// folder, as with a single repo per host.
	Owns func(folder string) bool
}

// owns reports whether the repo may touch folder.
func (r Roots) owns(folder string) bool { return r.Owns == nil || r.Owns(folder) }

// MemoryDirs lists every <hash>/memory directory found under either root.
func (r Roots) MemoryDirs() ([]string, error) {
	seen := map[string]struct{}{}
	for _, root := range []string{r.Claude, r.Mirror} {
		entries, err := os.ReadDir(root)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", root, err)
		}
		for _, e := range entries {
			if !e.IsDir() || !r.owns(e.Name()) {
				continue
			}
			seen[e.Name()] = struct{}{}
		}
	}
	dirs := make([]string, 0, len(seen))
	for hash := range seen {
		dirs = append(dirs, hash)
	}
	return dirs, nil
}

// SyncReport is returned by reconciliation operations for diagnostics.
type SyncReport struct {
	CopiedToMirror    []string // relative paths copied Claude → mirror
	CopiedToClaude    []string // relative paths copied mirror → Claude
	Merged            []string // MEMORY.md paths semantically merged
	BackedUp          []string // mirror copies preserved as .from-remote-* on collision
	RemovedFromMirror []string // user-deleted files removed from mirror (manifest-detected)
}

// Reconcile performs a bidirectional sync between the two trees, using the
// manifest to distinguish "user deleted this" from "this is new from
// another PC." Pass a nil or empty manifest to disable delete detection
// (the safe behavior for first-ever sync, since with no prior state we
// can't know what to delete).
//
// Decisions for each file path:
//
//   - Claude has, mirror has, identical → no-op
//   - Claude has, mirror has, differs    → MEMORY.md is merged via the
//     section-block merger; other files keep Claude and back the mirror
//     copy up as <name>.from-remote-<random>
//   - Claude has, mirror missing         → copy Claude → mirror
//   - Claude missing, mirror has, in manifest    → user deleted; remove
//     from mirror so the next git push propagates it
//   - Claude missing, mirror has, NOT in manifest → new from another PC;
//     copy mirror → Claude
//
// This function is used during `init` and as a periodic safety net on the
// pull tick. The watcher-driven sync loop also handles incremental events
// directly via CopyToMirror / RemoveFromMirror.
func Reconcile(r Roots, manifest *Manifest) (SyncReport, error) {
	rep := SyncReport{}
	pendingCT := 0
	defer func() {
		if pendingCT > 0 {
			log.Printf("reconcile: %d file(s) hold ciphertext this host cannot read; left pending", pendingCT)
		}
	}()
	if manifest == nil {
		manifest = NewManifest()
	}

	hashes, err := r.MemoryDirs()
	if err != nil {
		return rep, err
	}

	for _, hash := range hashes {
		claudeMem := filepath.Join(r.Claude, hash, "memory")
		mirrorMem := filepath.Join(r.Mirror, hash, "memory")
		if !realDirOrMissing(filepath.Join(r.Claude, hash)) || !realDirOrMissing(claudeMem) ||
			!realDirOrMissing(filepath.Join(r.Mirror, hash)) || !realDirOrMissing(mirrorMem) {
			continue // a link where a folder belongs is never followed
		}
		if err := os.MkdirAll(mirrorMem, 0700); err != nil {
			return rep, err
		}

		files := map[string]struct{}{}
		// Walk Claude side
		if entries, err := os.ReadDir(claudeMem); err == nil {
			for _, e := range entries {
				if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".") {
					warnNotRegular(filepath.Join(claudeMem, e.Name()), e.Type())
					continue
				}
				files[e.Name()] = struct{}{}
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return rep, fmt.Errorf("read claude %s: %w", claudeMem, err)
		}
		// Walk mirror side
		if entries, err := os.ReadDir(mirrorMem); err == nil {
			for _, e := range entries {
				if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".") {
					warnNotRegular(filepath.Join(mirrorMem, e.Name()), e.Type())
					continue
				}
				if strings.Contains(e.Name(), ".from-remote-") {
					continue
				}
				files[e.Name()] = struct{}{}
			}
		}

		for name := range files {
			cp := filepath.Join(claudeMem, name)
			mp := filepath.Join(mirrorMem, name)
			rel := filepath.ToSlash(filepath.Join(hash, "memory", name))
			if skipIfLinked(r, hash, name) {
				continue // present but not a regular file: neither missing nor ours to touch
			}

			cExists, cBytes := readIfExists(cp)
			mExists, mBytes := readIfExists(mp)
			if mExists && agecrypt.IsCiphertext(mBytes) {
				// Unreadable here: never copy it over, or merge it into, the
				// Claude tree. It stays pending until it can be decrypted.
				pendingCT++
				continue
			}
			wasInManifest := manifest.Has(rel)

			switch {
			case cExists && !mExists:
				if err := writeAtomic(mp, cBytes); err != nil {
					return rep, fmt.Errorf("write mirror %s: %w", mp, err)
				}
				rep.CopiedToMirror = append(rep.CopiedToMirror, rel)

			case !cExists && mExists && wasInManifest:
				// User deleted on this PC. Remove from mirror so the next
				// git commit/push propagates the deletion.
				if err := os.Remove(mp); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return rep, fmt.Errorf("remove mirror %s: %w", mp, err)
				}
				rep.RemovedFromMirror = append(rep.RemovedFromMirror, rel)

			case !cExists && mExists:
				// Not in manifest → new from another PC.
				if err := os.MkdirAll(claudeMem, 0700); err != nil {
					return rep, err
				}
				if err := writeAtomic(cp, mBytes); err != nil {
					return rep, fmt.Errorf("write claude %s: %w", cp, err)
				}
				rep.CopiedToClaude = append(rep.CopiedToClaude, rel)

			case cExists && mExists && bytes.Equal(cBytes, mBytes):
				// already matched — nothing to do

			case cExists && mExists && !bytes.Equal(cBytes, mBytes):
				merged, conflict := "", true
				if name == "MEMORY.md" {
					// The base is "" on purpose. The mirror copy is normally
					// HEAD's, and a base equal to it would make every section
					// "unchanged" on that side: the Claude copy would win
					// outright and the peer's sections would be dropped.
					merged, conflict = merge.Merge("", string(cBytes), string(mBytes))
				}
				if !conflict {
					if err := writeAtomic(cp, []byte(merged)); err != nil {
						return rep, err
					}
					if err := writeAtomic(mp, []byte(merged)); err != nil {
						return rep, err
					}
					rep.Merged = append(rep.Merged, rel)
				} else {
					// Any other file, or a MEMORY.md section edited on one side
					// only (with base "" that is a conflict, and the merge text
					// then holds conflict markers): the Claude copy wins and the
					// mirror copy is kept beside it.
					if err := writeBackup(mp, mBytes); err != nil {
						return rep, err
					}
					if err := writeAtomic(mp, cBytes); err != nil {
						return rep, err
					}
					rep.CopiedToMirror = append(rep.CopiedToMirror, rel)
					rep.BackedUp = append(rep.BackedUp, rel)
				}
			}
		}
	}
	return rep, nil
}

// writeBackup keeps content as mp.from-remote-<id>, unless a file of that name
// already holds the same bytes: the same incoming copy is then kept once, however
// many times it is set aside.
func writeBackup(mp string, content []byte) error {
	prefix := filepath.Base(mp) + ".from-remote-"
	if entries, err := os.ReadDir(filepath.Dir(mp)); err == nil {
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), prefix) {
				continue
			}
			if b, err := os.ReadFile(filepath.Join(filepath.Dir(mp), e.Name())); err == nil && bytes.Equal(b, content) {
				return nil
			}
		}
	}
	return writeAtomic(fmt.Sprintf("%s.from-remote-%s", mp, randSuffix()), content)
}

// CopyToMirror copies a single Claude-side file into the mirror, creating
// parent dirs as needed. Used by the watcher loop on a file change.
func CopyToMirror(r Roots, hash, name string) error {
	src := filepath.Join(r.Claude, hash, "memory", name)
	dst := filepath.Join(r.Mirror, hash, "memory", name)
	if _, err := os.Lstat(src); errors.Is(err, fs.ErrNotExist) {
		return err // the caller treats a vanished source as a delete (but see RemoveFromMirror)
	}
	if skipIfLinked(r, hash, name) {
		return nil
	}
	b, err := readRegular(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return err
	}
	return writeAtomic(dst, b)
}

// ErrCiphertext means a mirror file holds age ciphertext this host could not
// decrypt. It is never written into the Claude tree, where it would replace a
// note with unreadable bytes.
var ErrCiphertext = errors.New("file holds ciphertext this host cannot read; left pending")

// CopyToClaude copies a single mirror-side file back into Claude's tree.
// Used after a `git pull` brings in remote changes.
func CopyToClaude(r Roots, hash, name string) error {
	src := filepath.Join(r.Mirror, hash, "memory", name)
	dst := filepath.Join(r.Claude, hash, "memory", name)
	if _, err := os.Lstat(src); errors.Is(err, fs.ErrNotExist) {
		return err // the caller treats a vanished source as a delete (but see RemoveFromMirror)
	}
	if skipIfLinked(r, hash, name) {
		return nil
	}
	b, err := readRegular(src)
	if err != nil {
		return err
	}
	if agecrypt.IsCiphertext(b) {
		return ErrCiphertext
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return err
	}
	return writeAtomic(dst, b)
}

// RemoveFromMirror handles a Claude-side delete: remove the corresponding
// mirror file so the next commit propagates the deletion.
func RemoveFromMirror(r Roots, hash, name string) error {
	if skipIfLinked(r, hash, name, true) {
		return nil // a note replaced by a link is not a delete
	}
	target := filepath.Join(r.Mirror, hash, "memory", name)
	err := os.Remove(target)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func readIfExists(p string) (bool, []byte) {
	b, err := readRegular(p)
	if err != nil {
		return false, nil
	}
	return true, b
}

// errNotRegular means a path is a link, a directory or another special file
// where a note was expected. Links are never followed: a fetched tree or a
// local folder could point one at any file the user can read.
var errNotRegular = errors.New("not a regular file (links are not followed)")

// readRegular reads p only if it is a regular file, not a link to one.
func readRegular(p string) ([]byte, error) {
	st, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: %w", p, errNotRegular)
	}
	return os.ReadFile(p)
}

// realDirOrMissing reports whether p is absent or a real directory (not a link).
func realDirOrMissing(p string) bool {
	st, err := os.Lstat(p)
	if err != nil {
		return errors.Is(err, fs.ErrNotExist)
	}
	return st.IsDir()
}

// skipIfLinked reports whether a folder on the path, or the note itself on
// either side, exists but is not a real directory or a regular file (a link, a
// socket, ...). Such a name is skipped outright: it is not "missing" (so no
// deletion goes into the mirror), nothing is copied over it or out of it, and
// the manifest is left as it is. It warns once per path.
func skipIfLinked(r Roots, hash, name string, mirrorDirOK ...bool) bool {
	for _, d := range []string{
		filepath.Join(r.Claude, hash), filepath.Join(r.Claude, hash, "memory"),
		filepath.Join(r.Mirror, hash), filepath.Join(r.Mirror, hash, "memory"),
	} {
		if !realDirOrMissing(d) {
			warnSkipped(d)
			return true
		}
	}
	for i, p := range []string{
		filepath.Join(r.Claude, hash, "memory", name), filepath.Join(r.Mirror, hash, "memory", name),
	} {
		st, err := os.Lstat(p)
		if err != nil || st.Mode().IsRegular() {
			continue
		}
		// RemoveFromMirror may meet a directory at the mirror path: removing
		// it fails on its own, which is the error callers already handle.
		if i == 1 && st.IsDir() && len(mirrorDirOK) > 0 && mirrorDirOK[0] {
			continue
		}
		warnSkipped(p)
		return true
	}
	return false
}

func warnSkipped(p string) {
	if _, seen := warnedLinks.LoadOrStore(p, true); !seen {
		// Names stay out of the log: a path may match the denylist.
		log.Printf("reconcile: skipping a link or special entry in a memory folder; it is never followed, deleted or overwritten (names are not logged)")
	}
}

var warnedLinks sync.Map

// warnNotRegular logs once per path that a link (or other special entry) was
// skipped. Plain directories are not worth a line.
func warnNotRegular(p string, t fs.FileMode) {
	if t.IsDir() || t.IsRegular() {
		return
	}
	if _, seen := warnedLinks.LoadOrStore(p, true); !seen {
		log.Printf("reconcile: skipping a link or special entry in a memory folder; it is never followed (names are not logged)")
	}
}

// writeAtomic writes content to a unique sibling .tmp file then renames it
// over the destination. This avoids exposing partial content to a reader
// (e.g., Claude reading a memory while we're mid-write).
// renameFile is os.Rename; a variable so a test can make a write fail the same
// way on every OS (permission bits and directory modes do not do that on
// Windows).
var renameFile = os.Rename

func writeAtomic(path string, content []byte) error {
	dir := filepath.Dir(path)
	tmp := filepath.Join(dir, fmt.Sprintf(".%s.%s.tmp", filepath.Base(path), randSuffix()))
	if err := os.WriteFile(tmp, content, 0600); err != nil {
		return err
	}
	if err := renameFile(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func randSuffix() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// fall back to time
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
