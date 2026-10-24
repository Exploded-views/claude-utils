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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/config"
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

	// Remote, when non-nil, stores a project folder that holds a remote key
	// under remotes/<host>/<owner>/<repo>/ instead of projects/<hash>/. Nil
	// means every project is keyed by its path (the default).
	Remote *RemoteMap
}

// NewRoots builds the Roots for a config, resolving project keys when
// cfg.ProjectKey is "remote".
func NewRoots(cfg config.Config) (Roots, error) {
	r := Roots{Claude: cfg.ClaudeProjectsDir, Mirror: filepath.Join(cfg.SyncDir, "projects")}
	if cfg.ProjectKey == config.ProjectKeyRemote {
		m, err := NewRemoteMap(cfg.SyncDir, cfg.ClaudeProjectsDir)
		if err != nil {
			return r, err
		}
		r.Remote = m
	}
	return r, nil
}

// MirrorMemDir returns the repo-side memory directory for the local project
// folder named hash. It is the only place the two layouts differ: a folder
// holding a remote key lives under remotes/, every other folder under
// projects/<hash>/. Everything else treats both alike.
func (r Roots) MirrorMemDir(hash string) string {
	if r.Remote != nil {
		if key := r.Remote.Key(hash); key != "" {
			return r.Remote.MemoryDir(key)
		}
	}
	return filepath.Join(r.Mirror, hash, "memory")
}

// LogSuperseded logs, once per remote-keyed project folder, that a copy under
// projects/<hash>/memory is still in the sync repo but is no longer updated:
// the folder was path-keyed before projectKey was switched to remote. Nothing
// is migrated or removed; that stays a manual step once every PC has switched.
func (r Roots) LogSuperseded() {
	if r.Remote == nil {
		return
	}
	hashes := make([]string, 0, len(r.Remote.keys))
	for h := range r.Remote.keys {
		hashes = append(hashes, h)
	}
	sort.Strings(hashes)
	for _, h := range hashes {
		key := r.Remote.keys[h]
		if key != "" && isDir(filepath.Join(r.Mirror, h, "memory")) {
			logFolder(h, "now stored as remotes/%s; its old copy projects/%s/memory in the sync repo is no longer updated",
				key, h)
		}
	}
}

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
			if !e.IsDir() {
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
	BackedUp          []string // copies preserved as .from-remote-* (mirror) or .local-before-join-* (Claude's) on collision
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
	if manifest == nil {
		manifest = NewManifest()
	}

	hashes, err := r.MemoryDirs()
	if err != nil {
		return rep, err
	}
	if r.Remote != nil {
		r.Remote.forgetChanges()
	}

	for _, hash := range hashes {
		if r.folderVanished(hash) {
			continue // its notes are under remotes/ and are not touched here
		}
		claudeMem := filepath.Join(r.Claude, hash, "memory")
		mirrorMem := r.MirrorMemDir(hash)
		if err := os.MkdirAll(mirrorMem, 0700); err != nil {
			return rep, err
		}
		if r.joinPending(hash) {
			if err := r.joinFolder(hash, &rep); err != nil {
				return rep, err
			}
			continue
		}

		files := map[string]struct{}{}
		// Walk Claude side
		if entries, err := os.ReadDir(claudeMem); err == nil {
			for _, e := range entries {
				if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
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
				if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
					continue
				}
				if isBackupName(e.Name()) {
					continue
				}
				files[e.Name()] = struct{}{}
			}
		}

		for name := range files {
			cp := filepath.Join(claudeMem, name)
			mp := filepath.Join(mirrorMem, name)
			rel := filepath.ToSlash(filepath.Join(hash, "memory", name))

			cExists, cBytes := readIfExists(cp)
			mExists, mBytes := readIfExists(mp)
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
				// Not in manifest → new from another PC (or, in remote mode, a
				// leftover the folder has no business importing).
				if !r.mayImport(hash, name) {
					continue
				}
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
				if name == "MEMORY.md" {
					merged, _ := merge.Merge("", string(cBytes), string(mBytes))
					if err := writeAtomic(cp, []byte(merged)); err != nil {
						return rep, err
					}
					if err := writeAtomic(mp, []byte(merged)); err != nil {
						return rep, err
					}
					rep.Merged = append(rep.Merged, rel)
				} else {
					backup := fmt.Sprintf("%s.from-remote-%s", mp, randSuffix())
					if err := writeAtomic(backup, mBytes); err != nil {
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

// CopyToMirror copies a single Claude-side file into the mirror, creating
// parent dirs as needed. Used by the watcher loop on a file change.
func CopyToMirror(r Roots, hash, name string) error {
	if err := r.JoinIfNeeded(hash); err != nil {
		return err
	}
	src := filepath.Join(r.Claude, hash, "memory", name)
	dst := filepath.Join(r.MirrorMemDir(hash), name)
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return err
	}
	return writeAtomic(dst, b)
}

// CopyToClaude copies a single mirror-side file back into Claude's tree.
// Used after a `git pull` brings in remote changes.
func CopyToClaude(r Roots, hash, name string) error {
	src := filepath.Join(r.MirrorMemDir(hash), name)
	dst := filepath.Join(r.Claude, hash, "memory", name)
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return err
	}
	return writeAtomic(dst, b)
}

// RemoveFromMirror handles a Claude-side delete: remove the corresponding
// mirror file so the next commit propagates the deletion. Except when the whole
// Claude project folder of a remote-keyed folder is gone (an old clone's
// folder was deleted): its files' remove events say nothing about the notes,
// which are shared with other PCs. The flush restores them (see
// RemoteMap.Vanished); `rm -rf memory/` alone, with the project folder still
// there, is still a delete, as in path mode.
func RemoveFromMirror(r Roots, hash, name string) error {
	if r.folderVanished(hash) {
		return nil // the folder is gone, not its notes
	}
	if err := r.JoinIfNeeded(hash); err != nil {
		return err
	}
	target := filepath.Join(r.MirrorMemDir(hash), name)
	err := os.Remove(target)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// backupMarkers are in the name of the local-only conflict backups: the mirror
// copy Reconcile replaced (.from-remote-*) and the Claude copy a join replaced
// (.local-before-join-*). Both are git-ignored and never synced.
var backupMarkers = []string{".from-remote-", ".local-before-join-"}

func isBackupName(name string) bool {
	for _, m := range backupMarkers {
		if strings.Contains(name, m) {
			return true
		}
	}
	return false
}

// folderVanished reports whether hash holds a remote key but its Claude project
// folder no longer exists.
func (r Roots) folderVanished(hash string) bool {
	return r.Remote != nil && r.Remote.claimedKey(hash) != "" && !isDir(filepath.Join(r.Claude, hash))
}

// joinPending reports whether the folder has just gained its remote key (a new
// claim, a key change, a claim moved to it) and not yet been joined to the
// notes already stored under that key.
func (r Roots) joinPending(hash string) bool {
	if r.Remote == nil || r.Remote.Key(hash) == "" {
		return false
	}
	return r.Remote.join[hash]
}

// JoinIfNeeded joins the folder to its key's notes if it just gained the key.
// Every path that touches a folder's mirror directory calls it first, so the
// folder's own, possibly diverged, notes never overwrite the shared ones.
func (r Roots) JoinIfNeeded(hash string) error {
	if !r.joinPending(hash) {
		return nil
	}
	var rep SyncReport
	return r.joinFolder(hash, &rep)
}

// joinFolder reconciles one folder that has just gained a remote key with the
// notes already under that key, which another PC (or this folder's own earlier
// life under another key) may have written. Unlike Reconcile the shared side
// is authoritative and nothing is ever deleted:
//
//   - MEMORY.md is merged with internal/merge and written to both sides;
//   - another file that differs keeps the shared version in both places and
//     the local one is saved next to the shared copy as
//     <name>.local-before-join-<random> (git-ignored);
//   - a file only the shared side has is copied into Claude;
//   - a file only the folder has is copied into the shared side.
//
// The manifest is not consulted: the folder has no history with this key, so
// "missing here" says nothing about a delete.
func (r Roots) joinFolder(hash string, rep *SyncReport) error {
	claudeMem := filepath.Join(r.Claude, hash, "memory")
	mirrorMem := r.MirrorMemDir(hash)
	if err := os.MkdirAll(mirrorMem, 0700); err != nil {
		return err
	}
	r.Remote.forgetChanges()
	names := map[string]struct{}{}
	for _, dir := range []string{claudeMem, mirrorMem} {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") || isBackupName(e.Name()) {
				continue
			}
			names[e.Name()] = struct{}{}
		}
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	for _, name := range sorted {
		cp, mp := filepath.Join(claudeMem, name), filepath.Join(mirrorMem, name)
		rel := filepath.ToSlash(filepath.Join(hash, "memory", name))
		cExists, cBytes := readIfExists(cp)
		mExists, mBytes := readIfExists(mp)
		switch {
		case cExists && !mExists:
			if err := writeAtomic(mp, cBytes); err != nil {
				return fmt.Errorf("write mirror %s: %w", mp, err)
			}
			rep.CopiedToMirror = append(rep.CopiedToMirror, rel)
		case !cExists && mExists:
			if !r.mayImport(hash, name) {
				continue
			}
			if err := os.MkdirAll(claudeMem, 0700); err != nil {
				return err
			}
			if err := writeAtomic(cp, mBytes); err != nil {
				return fmt.Errorf("write claude %s: %w", cp, err)
			}
			rep.CopiedToClaude = append(rep.CopiedToClaude, rel)
		case bytes.Equal(cBytes, mBytes):
		case name == "MEMORY.md":
			merged, _ := merge.Merge("", string(cBytes), string(mBytes))
			if err := writeAtomic(cp, []byte(merged)); err != nil {
				return err
			}
			if err := writeAtomic(mp, []byte(merged)); err != nil {
				return err
			}
			rep.Merged = append(rep.Merged, rel)
		default:
			backup := fmt.Sprintf("%s.local-before-join-%s", mp, randSuffix())
			if err := writeAtomic(backup, cBytes); err != nil {
				return err
			}
			if err := writeAtomic(cp, mBytes); err != nil {
				return err
			}
			rep.BackedUp = append(rep.BackedUp, rel)
		}
	}
	r.Remote.joined(hash)
	return nil
}

// mayImport reports whether a file that only the mirror has may be copied into
// the folder.
func (r Roots) mayImport(hash, name string) bool {
	if r.Remote == nil {
		return true
	}
	return r.Remote.mayImport(hash, repoDir(r.Remote.Key(hash), hash), name)
}

func readIfExists(p string) (bool, []byte) {
	b, err := os.ReadFile(p)
	if err != nil {
		return false, nil
	}
	return true, b
}

// writeAtomic writes content to a unique sibling .tmp file then renames it
// over the destination. This avoids exposing partial content to a reader
// (e.g., Claude reading a memory while we're mid-write).
func writeAtomic(path string, content []byte) error {
	dir := filepath.Dir(path)
	tmp := filepath.Join(dir, fmt.Sprintf(".%s.%s.tmp", filepath.Base(path), randSuffix()))
	if err := os.WriteFile(tmp, content, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
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
