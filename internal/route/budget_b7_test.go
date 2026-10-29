//go:build budget

package route

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// B7: the bytes read stay flat as the temp dir grows from 1k to 100k entries.
// A fresh process (a new Router) resolves one live checkout and a few gone
// folders; bytes are the entry-name bytes ReadDir returned for the big dir.
func TestBudgetB7_BytesReadFlatAsTmpdirGrows(t *testing.T) {
	run := func(entries int) (reads, bytes int) {
		big := filepath.Join(t.TempDir(), "T")
		if err := os.MkdirAll(filepath.Join(big, "live", "proj"), 0o700); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < entries; i++ {
			if err := os.WriteFile(filepath.Join(big, fmt.Sprintf("tmp.%06d", i)), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		restore := SetReadDirForTest(func(dir string, n int) ([]os.DirEntry, error) {
			es, err := ReadDirN(dir, n)
			if dir == big {
				reads++
				for _, e := range es {
					bytes += len(e.Name())
				}
			}
			return es, err
		})
		defer restore()
		r := New(File{}, func(string) bool { return true })
		r.Candidates(escape(filepath.Join(big, "live", "proj")))
		for i := 0; i < 5; i++ {
			r.Candidates(escape(filepath.Join(big, fmt.Sprintf("gone%d", i), "sub")))
		}
		return reads, bytes
	}
	r1, b1 := run(1000)
	r2, b2 := run(100000)
	t.Logf("B7 bytes read of the temp dir: 1k entries: %d reads, %d bytes; 100k entries: %d reads, %d bytes", r1, b1, r2, b2)
	// Flat: both are the first maxList+1 names; which names are first differs a
	// little with the directory's order, so allow half again.
	if b2 > b1*3/2 {
		t.Errorf("B7: bytes read grew from %d to %d as the temp dir grew 100x", b1, b2)
	}
}
