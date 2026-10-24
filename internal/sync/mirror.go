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

	// Remote, when non-nil, stores projects that have a git origin under
	// remotes/<host>/<owner>/<repo>/ instead of projects/<hash>/. Nil means
	// every project is keyed by its path (the default).
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
// folder named hash.
func (r Roots) MirrorMemDir(hash string) string {
	if r.Remote != nil {
		if key := r.Remote.Key(hash); key != "" {
			return r.Remote.MemoryDir(key)
		}
	}
	return filepath.Join(r.Mirror, hash, "memory")
}

// group is one repo-side memory directory and the local project folders that
// share it: exactly one in path mode, one or more in remote mode (a repo and
// its worktrees, or two clones).
type group struct {
	mirrorMem string
	locals    []string // project folder names under Claude
}

// groups pairs every local project folder, and every mirror-only path-keyed
// folder (new from another PC), with its repo-side directory.
func (r Roots) groups() ([]group, error) {
	byMirror := map[string]*group{}
	var order []string
	add := func(hash, mm string) {
		g, ok := byMirror[mm]
		if !ok {
			g = &group{mirrorMem: mm}
			byMirror[mm] = g
			order = append(order, mm)
		}
		g.locals = append(g.locals, hash)
	}
	local := map[string]bool{}
	for _, root := range []string{r.Claude, r.Mirror} {
		entries, err := os.ReadDir(root)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", root, err)
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, e := range entries {
			if !e.IsDir() || (root == r.Mirror && local[e.Name()]) {
				continue
			}
			if root == r.Claude {
				local[e.Name()] = true
				add(e.Name(), r.MirrorMemDir(e.Name()))
			} else {
				add(e.Name(), filepath.Join(r.Mirror, e.Name(), "memory"))
			}
		}
	}
	out := make([]group, 0, len(order))
	for _, mm := range order {
		out = append(out, *byMirror[mm])
	}
	return out, nil
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
	if manifest == nil {
		manifest = NewManifest()
	}

	groups, err := r.groups()
	if err != nil {
		return rep, err
	}
	for _, g := range groups {
		if err := reconcileGroup(r, g, manifest, &rep); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// localCopy is one local project folder's version of a memory file.
type localCopy struct {
	hash  string
	path  string
	rel   string // manifest key: <hash>/memory/<name>
	have  bool
	bytes []byte
	mtime time.Time
}

// reconcileGroup applies the Reconcile rules to one repo-side directory and
// the local folders that map to it. With a single local folder (always the
// case in path mode) it is the plain two-way rule set documented on
// Reconcile. With several (a repo and its worktrees in remote mode) the
// folders are unioned, like MEMORY.md sections:
//
//   - a file any folder has is copied to the others and to the mirror
//   - MEMORY.md versions are all merged together
//   - other files that differ keep the most recently modified version; every
//     losing version is preserved as <name>.from-remote-<random> in the mirror
//   - the mirror copy is deleted only when no folder has the file any more and
//     the manifest shows at least one folder used to (a delete in just one of
//     several folders is undone: it cannot tell "deleted" from "never synced
//     into this worktree")
func reconcileGroup(r Roots, g group, manifest *Manifest, rep *SyncReport) error {
	if err := os.MkdirAll(g.mirrorMem, 0700); err != nil {
		return err
	}

	names := map[string]struct{}{}
	for _, h := range g.locals {
		dir := filepath.Join(r.Claude, h, "memory")
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read claude %s: %w", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			names[e.Name()] = struct{}{}
		}
	}
	if entries, err := os.ReadDir(g.mirrorMem); err == nil {
		for _, e := range entries {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") || strings.Contains(e.Name(), ".from-remote-") {
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
		mp := filepath.Join(g.mirrorMem, name)
		mExists, mBytes := readIfExists(mp)

		locals := make([]localCopy, len(g.locals))
		var present []int // indexes into locals
		known := false    // the manifest remembers this file in some folder
		for i, h := range g.locals {
			lc := localCopy{hash: h, path: filepath.Join(r.Claude, h, "memory", name)}
			lc.rel = filepath.ToSlash(filepath.Join(h, "memory", name))
			lc.have, lc.bytes = readIfExists(lc.path)
			if lc.have {
				if info, err := os.Stat(lc.path); err == nil {
					lc.mtime = info.ModTime()
				}
				present = append(present, i)
			}
			known = known || manifest.Has(lc.rel)
			locals[i] = lc
		}

		toClaude := func(lc localCopy, content []byte) error {
			if err := os.MkdirAll(filepath.Dir(lc.path), 0700); err != nil {
				return err
			}
			if err := writeAtomic(lc.path, content); err != nil {
				return fmt.Errorf("write claude %s: %w", lc.path, err)
			}
			rep.CopiedToClaude = append(rep.CopiedToClaude, lc.rel)
			return nil
		}

		switch {
		case len(present) == 0 && known:
			// User deleted on this PC. Remove from mirror so the next
			// git commit/push propagates the deletion.
			if err := os.Remove(mp); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("remove mirror %s: %w", mp, err)
			}
			rep.RemovedFromMirror = append(rep.RemovedFromMirror, locals[0].rel)

		case len(present) == 0:
			// Not in manifest → new from another PC.
			for _, lc := range locals {
				if err := toClaude(lc, mBytes); err != nil {
					return err
				}
			}

		default:
			if err := reconcilePresent(name, mp, mExists, mBytes, locals, present, toClaude, rep); err != nil {
				return err
			}
		}
	}
	return nil
}

// reconcilePresent handles a file that at least one local folder has.
func reconcilePresent(name, mp string, mExists bool, mBytes []byte, locals []localCopy, present []int,
	toClaude func(localCopy, []byte) error, rep *SyncReport) error {

	first := locals[present[0]]
	same := true
	for _, i := range present {
		same = same && bytes.Equal(locals[i].bytes, first.bytes)
	}
	same = same && (!mExists || bytes.Equal(mBytes, first.bytes))

	var final []byte
	switch {
	case same:
		final = first.bytes
		if !mExists {
			if err := writeAtomic(mp, final); err != nil {
				return fmt.Errorf("write mirror %s: %w", mp, err)
			}
			rep.CopiedToMirror = append(rep.CopiedToMirror, first.rel)
		}

	case name == "MEMORY.md":
		merged := string(first.bytes)
		for _, i := range present[1:] {
			merged, _ = merge.Merge("", merged, string(locals[i].bytes))
		}
		if mExists {
			merged, _ = merge.Merge("", merged, string(mBytes))
		}
		final = []byte(merged)
		if err := writeAtomic(mp, final); err != nil {
			return err
		}
		rep.Merged = append(rep.Merged, first.rel)

	default:
		winner := first
		for _, i := range present[1:] {
			if locals[i].mtime.After(winner.mtime) {
				winner = locals[i]
			}
		}
		final = winner.bytes
		backup := func(content []byte) error {
			return writeAtomic(fmt.Sprintf("%s.from-remote-%s", mp, randSuffix()), content)
		}
		if mExists && !bytes.Equal(mBytes, final) {
			if err := backup(mBytes); err != nil {
				return err
			}
			rep.BackedUp = append(rep.BackedUp, winner.rel)
		}
		for _, i := range present {
			if !bytes.Equal(locals[i].bytes, final) {
				if err := backup(locals[i].bytes); err != nil {
					return err
				}
				rep.BackedUp = append(rep.BackedUp, locals[i].rel)
			}
		}
		if !mExists || !bytes.Equal(mBytes, final) {
			if err := writeAtomic(mp, final); err != nil {
				return err
			}
			rep.CopiedToMirror = append(rep.CopiedToMirror, winner.rel)
		}
	}

	// Bring every local folder up to the final content.
	for _, lc := range locals {
		if lc.have && bytes.Equal(lc.bytes, final) {
			continue
		}
		if err := toClaude(lc, final); err != nil {
			return err
		}
	}
	return nil
}

// CopyToMirror copies a single Claude-side file into the mirror, creating
// parent dirs as needed. Used by the watcher loop on a file change.
func CopyToMirror(r Roots, hash, name string) error {
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
// mirror file so the next commit propagates the deletion.
func RemoveFromMirror(r Roots, hash, name string) error {
	target := filepath.Join(r.MirrorMemDir(hash), name)
	err := os.Remove(target)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
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
