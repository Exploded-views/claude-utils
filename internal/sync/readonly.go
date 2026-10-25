package sync

import (
	"errors"
	"os"
	"path/filepath"
)

// ErrReadOnly is the refusal of every commit and push path on a read-only host.
var ErrReadOnly = errors.New("this host is read-only: it pulls notes but never commits or pushes")

// PopulateClaude copies every note of the mirror into the Claude tree where the
// Claude copy is missing or differs, and returns how many it wrote. A read-only
// host uses it in place of Reconcile, which would also send Claude-side notes
// the other way. Ciphertext this host cannot read is left pending, as in
// propagateChanges.
func PopulateClaude(r Roots) (int, error) {
	hashes, err := r.MemoryDirs()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, hash := range hashes {
		dir := filepath.Join(r.Mirror, hash, "memory")
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() || shouldIgnoreFile(e.Name()) {
				continue
			}
			src, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				return n, err
			}
			if cur, err := os.ReadFile(filepath.Join(r.Claude, hash, "memory", e.Name())); err == nil && string(cur) == string(src) {
				continue
			}
			switch err := CopyToClaude(r, hash, e.Name()); {
			case errors.Is(err, ErrCiphertext):
				continue
			case err != nil:
				return n, err
			}
			n++
		}
	}
	return n, nil
}
