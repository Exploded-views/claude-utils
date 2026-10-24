package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
)

// SyncState is this PC's record of the last content it synced for each file of
// a remote-keyed group (one repo-side memory directory shared by the local
// clones of one remote). It is the merge base for reconcileMulti.
//
// HEAD cannot be that base. It is ahead of what the clones hold after a pull
// that was not yet propagated (a crash, or a failed CopyToClaude), and behind
// when files are held back from commits, so judging "stale" or "deleted"
// against it reverts remote edits, deletes and MEMORY.md pruning. What this PC
// last brought every member to is the only base that is right on both counts.
//
// A group is keyed by its repo-side directory relative to the sync dir
// ("remotes/github.com/acme/app/memory"). The hash of each file is stored in
// .state/sync-state.json; for the MEMORY.md index the bytes are also kept in
// .state/base/<group>/MEMORY.md for the three-way merge. Plain files in
// .state/ rather than git blobs (git hash-object -w): a loose blob nothing
// refers to is pruned by gc, and there is no need to shell out per file. Like
// the manifest, .state/ is git-ignored, so the record is per PC.
type SyncState struct {
	syncDir string
	groups  map[string]map[string]string // group -> file name -> hex sha256
	dirty   bool
}

type syncStateJSON struct {
	Version int                          `json:"version"`
	Groups  map[string]map[string]string `json:"groups"`
}

const (
	stateFileName = "sync-state.json"
	stateBaseDir  = "base"
	stateVersion  = 1
	indexFile     = "MEMORY.md" // the file whose base bytes are kept
)

func newSyncState(syncDir string) *SyncState {
	return &SyncState{syncDir: syncDir, groups: map[string]map[string]string{}}
}

// LoadSyncState reads the record. A missing file is an empty record. So is one
// that cannot be read or parsed, after a log line: an empty record never
// deletes anything, so the worst case is a union instead of a delete.
func LoadSyncState(syncDir string) *SyncState {
	s := newSyncState(syncDir)
	p := filepath.Join(syncDir, manifestStateDir, stateFileName)
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return s
	}
	if err != nil {
		log.Printf("sync state: %v; continuing without delete detection for shared folders", err)
		return s
	}
	var raw syncStateJSON
	if err := json.Unmarshal(b, &raw); err != nil || raw.Version != stateVersion {
		log.Printf("sync state: %s is unusable (%v); starting with an empty record", p, err)
		return s
	}
	for g, files := range raw.Groups {
		if len(files) > 0 {
			s.groups[g] = files
		}
	}
	return s
}

func contentHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (s *SyncState) basePath(group, name string) string {
	return filepath.Join(s.syncDir, manifestStateDir, stateBaseDir, filepath.FromSlash(group), name)
}

// Hash returns the recorded content hash of a file. Safe on a nil state, which
// records nothing (path mode).
func (s *SyncState) Hash(group, name string) (string, bool) {
	if s == nil {
		return "", false
	}
	h, ok := s.groups[group][name]
	return h, ok
}

// Names lists the files recorded for a group.
func (s *SyncState) Names(group string) []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.groups[group]))
	for n := range s.groups[group] {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Base returns the recorded bytes of the index, or nil when there are none or
// they no longer match the recorded hash (a crash between the base write and
// the state write): then the merge uses an empty base.
func (s *SyncState) Base(group, name string) []byte {
	h, ok := s.Hash(group, name)
	if !ok || name != indexFile {
		return nil
	}
	b, err := os.ReadFile(s.basePath(group, name))
	if err != nil || contentHash(b) != h {
		return nil
	}
	return b
}

// Record notes content as the last synced version of a file.
func (s *SyncState) Record(group, name string, content []byte) error {
	if s == nil {
		return nil
	}
	h := contentHash(content)
	if old, ok := s.groups[group][name]; ok && old == h && (name != indexFile || s.Base(group, name) != nil) {
		return nil
	}
	if name == indexFile {
		bp := s.basePath(group, name)
		if err := os.MkdirAll(filepath.Dir(bp), 0700); err != nil {
			return err
		}
		if err := writeAtomic(bp, content); err != nil {
			return err
		}
	}
	if s.groups[group] == nil {
		s.groups[group] = map[string]string{}
	}
	s.groups[group][name] = h
	s.dirty = true
	return nil
}

// Forget drops a file's record and base bytes.
func (s *SyncState) Forget(group, name string) {
	if s == nil {
		return
	}
	if _, ok := s.groups[group][name]; !ok {
		return
	}
	delete(s.groups[group], name)
	if len(s.groups[group]) == 0 {
		delete(s.groups, group)
	}
	if name == indexFile {
		os.Remove(s.basePath(group, name))
	}
	s.dirty = true
}

// Save writes the record atomically if it changed.
func (s *SyncState) Save() error {
	if s == nil || !s.dirty {
		return nil
	}
	b, err := json.MarshalIndent(syncStateJSON{Version: stateVersion, Groups: s.groups}, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	p := filepath.Join(s.syncDir, manifestStateDir, stateFileName)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	if err := writeAtomic(p, b); err != nil {
		return err
	}
	s.dirty = false
	return nil
}
