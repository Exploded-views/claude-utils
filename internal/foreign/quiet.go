package foreign

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Target is one Claude projects dir and the folders of it that a repo owns
// (nil Owns: all of them).
type Target struct {
	Claude string
	Owns   func(folder string) bool
}

type fileState struct {
	size  int64
	mtime time.Time
	sum   [32]byte
}

// snap maps folder -> memory file -> state, per target.
type snap []map[string]map[string]fileState

func hash8(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:4])
}

func snapshot(ts []Target) (snap, error) {
	out := make(snap, len(ts))
	for i, t := range ts {
		out[i] = map[string]map[string]fileState{}
		ents, err := os.ReadDir(t.Claude)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, e := range ents {
			if !e.IsDir() || (t.Owns != nil && !t.Owns(e.Name())) {
				continue
			}
			files := map[string]fileState{}
			mem := filepath.Join(t.Claude, e.Name(), "memory")
			err := filepath.WalkDir(mem, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					if errors.Is(err, fs.ErrNotExist) {
						return nil
					}
					return err
				}
				if d.IsDir() {
					return nil
				}
				b, err := os.ReadFile(p)
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				if err != nil {
					return err
				}
				st, err := os.Stat(p)
				if err != nil {
					if errors.Is(err, fs.ErrNotExist) {
						return nil
					}
					return err
				}
				rel, _ := filepath.Rel(mem, p)
				files[filepath.ToSlash(rel)] = fileState{st.Size(), st.ModTime(), sha256.Sum256(b)}
				return nil
			})
			if err != nil {
				return nil, err
			}
			out[i][e.Name()] = files
		}
	}
	return out, nil
}

// changedFolders names the folders (as hashes) that differ between two
// snapshots.
func changedFolders(a, b snap) []string {
	seen := map[string]bool{}
	for i := range a {
		for folder, fa := range a[i] {
			if !sameFiles(fa, b[i][folder]) {
				seen[hash8(folder)] = true
			}
		}
		for folder, fb := range b[i] {
			if _, ok := a[i][folder]; !ok && len(fb) > 0 {
				seen[hash8(folder)] = true
			}
		}
	}
	var out []string
	for h := range seen {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

func sameFiles(a, b map[string]fileState) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		w, ok := b[k]
		if !ok || v.size != w.size || !v.mtime.Equal(w.mtime) || v.sum != w.sum {
			return false
		}
	}
	return true
}

// QuietCheck snapshots the owned folders' memory files, waits secs seconds with
// wait, and snapshots again. Any difference means something else writes there,
// and it is an error naming the folders by hash. secs <= 0 disables it.
func QuietCheck(ts []Target, secs int, wait func(time.Duration)) error {
	if secs <= 0 {
		return nil
	}
	before, err := snapshot(ts)
	if err != nil {
		return fmt.Errorf("quiet window: %w", err)
	}
	wait(time.Duration(secs) * time.Second)
	after, err := snapshot(ts)
	if err != nil {
		return fmt.Errorf("quiet window: %w", err)
	}
	if ch := changedFolders(before, after); len(ch) > 0 {
		return fmt.Errorf("owned folder changed during start-up quiet window: something else writes there (folder %s)", strings.Join(ch, ", "))
	}
	return nil
}
