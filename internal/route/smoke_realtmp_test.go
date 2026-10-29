package route

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/testenv"
)

// Opt-in smoke test (MEMSYNC_REAL_TMPDIR_SMOKE=1) against the host's original
// TMPDIR, which the suites swap for a small directory: the real path (on macOS
// /var -> /private/var) at its real size. It is read-only: it creates, writes
// and deletes nothing there, and resolves only names that do not exist.
// It asserts what holds at any size: no read of a directory delivers more than
// maxList+1 entries, none lists in full, and a dashed name under a directory
// too big to list has no candidate and says so. On a small host TMPDIR the
// dashed name simply finds nothing, because nothing is there.
func TestRealHostTmpdirResolutionIsFlatCostAndFailsClosed(t *testing.T) {
	if os.Getenv("MEMSYNC_REAL_TMPDIR_SMOKE") != "1" {
		t.Skip("opt-in: set MEMSYNC_REAL_TMPDIR_SMOKE=1 to resolve names under the real host TMPDIR (read-only)")
	}
	if runtime.GOOS == "windows" {
		t.Skip("windows lists every directory in full by design")
	}
	host := filepath.Clean(testenv.HostTempDir())
	if host == filepath.Clean(os.TempDir()) {
		t.Logf("host TMPDIR %s was not swapped", host)
	}
	hostLarge := false
	if f, err := os.Open(host); err == nil {
		names, _ := f.Readdirnames(maxList + 1)
		f.Close()
		hostLarge = len(names) > maxList
	} else {
		t.Skipf("host TMPDIR %s unreadable: %v", host, err)
	}

	var mu sync.Mutex
	maxRead, reads, full := 0, 0, 0
	restore := SetReadDirForTest(func(dir string, n int) ([]os.DirEntry, error) {
		es, err := ReadDirN(dir, n)
		mu.Lock()
		reads++
		if n <= 0 {
			full++
		}
		if len(es) > maxRead {
			maxRead = len(es)
		}
		mu.Unlock()
		return es, err
	})
	defer restore()

	list := func(dir string) *listing { return readListing(dir) }
	dashed := escape(filepath.Join(host, "memsync-smoke-no-such-dir"))
	plain := escape(filepath.Join(host, "memsyncsmokenosuchdir"))

	got, large := candidates(dashed, list)
	if len(got) != 0 {
		t.Errorf("dashed name resolved to %v, want none", got)
	}
	if hostLarge && !large {
		t.Errorf("host TMPDIR has more than %d entries but the dashed name did not report large", maxList)
	}
	if got := Candidates(dashed); len(got) != 0 {
		t.Errorf("Candidates(dashed) = %v, want none", got)
	}
	if got, _ := candidates(plain, list); len(got) != 0 {
		t.Errorf("undashed missing name resolved to %v, want none", got)
	}
	self, _ := candidates(escape(host), list) // TMPDIR itself: only the bound is asserted
	if maxRead > maxList+1 {
		t.Errorf("a read delivered %d entries, want at most %d", maxRead, maxList+1)
	}
	if full != 0 {
		t.Errorf("%d full listings (n <= 0), want none", full)
	}
	t.Logf("SMOKE host=%s hostLarge=%v dashedLarge=%v reads=%d maxRead=%d fullListings=%d self=%v", host, hostLarge, large, reads, maxRead, full, self)
}
