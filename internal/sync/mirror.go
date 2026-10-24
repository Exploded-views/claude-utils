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
			log.Printf("remote keys: %s is now stored as remotes/%s; its old copy projects/%s/memory in the sync repo is no longer updated",
				h, key, h)
		}
	}
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
// share it: exactly one in path mode, one or more in remote mode (several
// clones of one remote on this PC).
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
				if r.Remote != nil && r.Remote.Key(e.Name()) != "" && !r.Remote.Member(e.Name()) {
					// Keyed only from memory (the checkout is gone, git will
					// not answer) and with no memory/ dir: nothing to share,
					// and not a folder to fill.
					continue
				}
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
	var st *SyncState // remote mode only: what this PC last synced per shared file
	if r.Remote != nil {
		if manifest == nil {
			st = newSyncState(r.Remote.syncDir) // no delete detection: ignore the old record
		} else {
			st = LoadSyncState(r.Remote.syncDir)
		}
		defer func() {
			if err := st.Save(); err != nil {
				log.Println("save sync state:", err)
			}
		}()
	}
	if manifest == nil {
		manifest = NewManifest()
	}

	groups, err := r.groups()
	if err != nil {
		return rep, err
	}
	for _, g := range groups {
		if err := reconcileGroup(r, g, manifest, st, &rep); err != nil {
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

// stateKey is the key of a group in SyncState: its repo-side directory
// relative to the sync dir. ok is false outside remote mode and for the
// path-keyed folders in it, which are not recorded.
func (r Roots) stateKey(mirrorMem string) (key string, ok bool) {
	if r.Remote == nil {
		return "", false
	}
	rel, err := filepath.Rel(r.Remote.syncDir, mirrorMem)
	if err != nil {
		return "", false
	}
	key = filepath.ToSlash(rel)
	return key, strings.HasPrefix(key, remotesDir+"/")
}

// recordFile notes the mirror copy of a file as what this PC last synced, or
// forgets the file when the mirror no longer has it. Call it only after the
// file's local folders have been brought to the mirror copy.
func recordFile(st *SyncState, gkey, mirrorMem, name string) {
	exists, b := readIfExists(filepath.Join(mirrorMem, name))
	if !exists {
		st.Forget(gkey, name)
		return
	}
	if err := st.Record(gkey, name, b); err != nil {
		log.Printf("record %s/%s: %v", gkey, name, err)
	}
}

// reconcileGroup applies the Reconcile rules to one repo-side directory and
// the local folders that map to it. With a single local folder (always the
// case in path mode) it is the plain two-way rule set documented on
// Reconcile. With several (two clones of one remote on this PC) each file goes through reconcileMulti.
func reconcileGroup(r Roots, g group, manifest *Manifest, st *SyncState, rep *SyncReport) error {
	if r.Remote != nil {
		// A folder whose memory/ dir is gone, though the manifest had files
		// in it, has left the group: someone removed the directory (or the
		// whole project folder), which says nothing about each file in it.
		// Deleting them from the shared copy would wipe every clone's notes.
		// Path mode keeps main's behavior. In remote mode the folder is
		// filled again by a later pass, once the manifest forgets it.
		stay := g.locals[:0:0]
		for _, h := range g.locals {
			if isDir(filepath.Join(r.Claude, h, "memory")) || !manifest.HasFolder(h) {
				stay = append(stay, h)
			}
		}
		if len(stay) == 0 {
			return nil
		}
		g.locals = stay
	}
	if err := os.MkdirAll(g.mirrorMem, 0700); err != nil {
		return err
	}
	gkey, recorded := r.stateKey(g.mirrorMem)

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
		if err := reconcileName(r, g, name, manifest, st, gkey, rep); err != nil {
			return err
		}
	}
	if recorded {
		// Everything above succeeded, so every member now holds the mirror
		// copy: that is what this PC has synced.
		for _, name := range sorted {
			recordFile(st, gkey, g.mirrorMem, name)
		}
		for _, name := range st.Names(gkey) {
			if _, ok := names[name]; !ok {
				st.Forget(gkey, name)
			}
		}
	}
	return nil
}

// reconcileName reconciles one file of a group. The state is nil, and gkey
// empty, where nothing is recorded.
func reconcileName(r Roots, g group, name string, manifest *Manifest, st *SyncState, gkey string, rep *SyncReport) error {
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

	if len(g.locals) > 1 {
		return reconcileMulti(st, gkey, mp, mExists, mBytes, locals, present, manifest, toClaude, rep)
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
		return reconcilePresent(name, mp, mExists, mBytes, locals, present, toClaude, rep)
	}
	return nil
}

// reconcileMulti reconciles one file across several local folders that share
// a repo-side copy. A union cannot tell "stale copy" from "new edit" nor
// "deleted here" from "never received", so it would undo deletes and
// MEMORY.md pruning. The base is this PC's record of what it last synced for
// the file (see SyncState), not the sync repo's HEAD. A copy "changed" when its
// hash differs from the record. The mirror copy is one more version: a pull
// leaves its result there, and so does the watcher.
//
//   - a copy equal to the record is stale and just takes the changed side,
//     with no backup
//   - the file is deleted when the mirror lacks it or a folder lacks what the
//     manifest remembers there, and no copy is changed; the delete wins over
//     stale copies, but a changed copy is an edit and is restored to
//     whoever lost it
//   - changed copies that differ from each other really diverged: MEMORY.md is
//     merged three-way against the recorded index, anything else keeps the
//     most recently modified copy and backs the others up as
//     <name>.from-remote-<random>. A pulled copy counts as oldest
//
// With no record (first run, or a file this PC never synced) nothing can be
// judged stale or deleted: it falls back to reconcilePresent's union, and the
// mirror copy is brought to folders that lack it.
func reconcileMulti(st *SyncState, gkey string, mp string, mExists bool, mBytes []byte, locals []localCopy, present []int,
	manifest *Manifest, toClaude func(localCopy, []byte) error, rep *SyncReport) error {

	name := filepath.Base(mp)
	rec, haveRec := st.Hash(gkey, name)
	if !haveRec {
		if len(present) == 0 {
			if !mExists {
				return nil
			}
			for _, lc := range locals {
				if err := toClaude(lc, mBytes); err != nil {
					return err
				}
			}
			return nil
		}
		return reconcilePresent(name, mp, mExists, mBytes, locals, present, toClaude, rep)
	}

	changed := func(b []byte) bool { return contentHash(b) != rec }
	edited := mExists && changed(mBytes)
	gone := !mExists
	for _, lc := range locals {
		if lc.have {
			edited = edited || changed(lc.bytes)
		} else {
			gone = gone || manifest.Has(lc.rel)
		}
	}
	if gone && !edited {
		for _, i := range present {
			if err := os.Remove(locals[i].path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("remove claude %s: %w", locals[i].path, err)
			}
		}
		if err := os.Remove(mp); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove mirror %s: %w", mp, err)
		}
		rep.RemovedFromMirror = append(rep.RemovedFromMirror, locals[0].rel)
		return nil
	}

	if len(present) == 0 {
		// Only the mirror has it (and it is an edit): restore it everywhere.
		for _, lc := range locals {
			if err := toClaude(lc, mBytes); err != nil {
				return err
			}
		}
		return nil
	}

	// Distinct versions that differ from the record: the changed sides.
	type side struct {
		content []byte
		mtime   time.Time
		rel     string
	}
	var sides []side
	addSide := func(c []byte, mt time.Time, rel string) {
		if !changed(c) {
			return
		}
		for k := range sides {
			if bytes.Equal(sides[k].content, c) {
				if mt.After(sides[k].mtime) {
					sides[k].mtime = mt
				}
				return
			}
		}
		sides = append(sides, side{c, mt, rel})
	}
	for _, i := range present {
		addSide(locals[i].bytes, locals[i].mtime, locals[i].rel)
	}
	if mExists {
		addSide(mBytes, time.Time{}, "(sync repo)/"+name)
	}

	var final []byte
	switch {
	case len(sides) == 0:
		final = locals[present[0]].bytes // every copy equals the record
	case len(sides) == 1:
		final = sides[0].content
	case name == indexFile:
		base := string(st.Base(gkey, name))
		merged := string(sides[0].content)
		for _, s := range sides[1:] {
			merged, _ = merge.Merge(base, merged, string(s.content))
		}
		final = []byte(merged)
		rep.Merged = append(rep.Merged, locals[present[0]].rel)
	default:
		w := 0
		for k := range sides {
			if sides[k].mtime.After(sides[w].mtime) {
				w = k
			}
		}
		final = sides[w].content
		for k, s := range sides {
			if k == w {
				continue
			}
			if err := writeAtomic(fmt.Sprintf("%s.from-remote-%s", mp, randSuffix()), s.content); err != nil {
				return err
			}
			rep.BackedUp = append(rep.BackedUp, s.rel)
		}
	}

	if !mExists || !bytes.Equal(mBytes, final) {
		if err := writeAtomic(mp, final); err != nil {
			return fmt.Errorf("write mirror %s: %w", mp, err)
		}
		rep.CopiedToMirror = append(rep.CopiedToMirror, locals[present[0]].rel)
	}
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

// reconcilePresent handles a file that at least one local folder has, by
// union (no merge base known).
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
