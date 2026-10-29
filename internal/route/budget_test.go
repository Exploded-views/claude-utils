//go:build budget

package route

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// B4: the directory reads for a fixed set of folders do not grow when the
// unrelated siblings of the directories on their paths do (10 to 2,000, in the
// big dir and in its parent). Reads are counted by calls and by entries
// delivered, which is the work proportional to the unrelated names.
func TestBudgetB4_NoWorkProportionalToUnrelatedDirs(t *testing.T) {
	type tally struct{ calls, entries int }
	run := func(siblings int) tally {
		root := t.TempDir()
		parent := filepath.Join(root, "p")
		big := filepath.Join(parent, "T")
		for _, d := range []string{parent, big} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < siblings; i++ {
			for _, d := range []string{parent, big} {
				if err := os.Mkdir(filepath.Join(d, fmt.Sprintf("unrelated%05d", i)), 0o700); err != nil {
					t.Fatal(err)
				}
			}
		}
		var folders []string
		for i := 0; i < 20; i++ {
			folders = append(folders, escape(filepath.Join(big, fmt.Sprintf("gone%02d", i), "sub")))
		}
		for i := 0; i < 5; i++ {
			d := filepath.Join(big, fmt.Sprintf("real%d", i))
			if err := os.Mkdir(d, 0o700); err != nil {
				t.Fatal(err)
			}
			folders = append(folders, escape(d))
		}
		var mu sync.Mutex
		var got tally
		restore := SetReadDirForTest(func(dir string) ([]os.DirEntry, error) {
			es, err := os.ReadDir(dir)
			if dir == big || dir == parent {
				mu.Lock()
				got.calls++
				got.entries += len(es)
				mu.Unlock()
			}
			return es, err
		})
		defer restore()
		r := New(File{}, func(string) bool { return true })
		for _, f := range folders {
			r.Candidates(f)
		}
		return got
	}
	small, large := run(10), run(2000)
	t.Logf("B4 reads of the two dirs: 10 siblings: %d calls, %d entries; 2000 siblings: %d calls, %d entries",
		small.calls, small.entries, large.calls, large.entries)
	if small.calls != large.calls {
		t.Errorf("B4: read calls grew from %d to %d with unrelated siblings", small.calls, large.calls)
	}
	if small.entries != large.entries {
		t.Errorf("B4: entries read grew from %d to %d with unrelated siblings", small.entries, large.entries)
	}
}
