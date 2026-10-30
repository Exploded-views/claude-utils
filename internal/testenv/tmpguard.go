package testenv

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/MarimerLLC/claude-utils/internal/listlimit"
)

// smallBase is the most entries the base dir itself may hold: a fixture made
// directly under a busier one would be slow to list for no reason.
const smallBase = 200

// readCap is the most names countEntries reads: one past the larger limit.
const readCap = listlimit.Max + 1

// tempCandidates are the bases SmallTempDir may use, in order: the temp dir the
// process started with, then /tmp, then /var/tmp. A test replaces it.
var tempCandidates = func() []string {
	if v := os.Getenv(EnvTmpCandidates); v != "" {
		return filepath.SplitList(v)
	}
	return []string{os.TempDir(), "/tmp", "/var/tmp"}
}

// EnvTmpCandidates replaces the candidate list with a path list (as PATH is
// spelled), for forcing every base bad when showing the INVALID skip.
const EnvTmpCandidates = "MEMSYNC_BUDGET_TMP_CANDIDATES"

// countEntries is the number of entries in dir. A test replaces it, so no
// check depends on the host's real directories.
var countEntries = func(dir string) (int, error) {
	f, err := os.Open(dir)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	names, err := f.Readdirnames(readCap)
	if err != nil && err != io.EOF {
		return 0, err
	}
	return len(names), nil
}

// invalidDirs are the directories whose ancestors the budget gates need small:
// the temp dir, and in prebuilt mode the temp HOME. A test replaces it.
var invalidDirs = func() []string {
	dirs := []string{os.TempDir()}
	if Prebuilt() {
		if h := os.Getenv("HOME"); h != "" {
			dirs = append(dirs, h)
		}
	}
	return dirs
}

// skipper is the part of testing.TB that SkipIfInvalid uses.
type skipper interface {
	Helper()
	Skipf(format string, args ...any)
}

// endT is the part of testing.TB that EndCheck uses.
type endT interface {
	Helper()
	Failed() bool
	Logf(format string, args ...any)
	Skipf(format string, args ...any)
}

// EndCheck is the last check of a counted gate, run from its cleanup, on the dirs
// SkipIfInvalid returned at the start (never the TMPDIR of the moment: B1 points
// it at its own big fixture). When an
// ancestor crossed the limit during the run, a passing gate is reported as
// INVALID (skipped). A gate that already failed stays failed; the INVALID line
// is logged beside the failure.
func EndCheck(t endT, dirs []string) {
	t.Helper()
	big, n, err := firstInvalid(dirs)
	switch {
	case big == "" && err == nil:
		return
	case t.Failed():
		t.Logf("%s", invalidMsg(big, n, err))
	default:
		t.Skipf("%s", invalidMsg(big, n, err))
	}
}

// RequireReads fails the gate when the walk made no reads at all: a gate that
// counts reads must not pass at 0 == 0.
func RequireReads(t errorer, what string, n int) {
	t.Helper()
	if n == 0 {
		t.Errorf("%s: the walk made 0 reads, so the gate measured nothing", what)
	}
}

// errorer is the part of testing.TB that RequireReads uses.
type errorer interface {
	Helper()
	Errorf(format string, args ...any)
}

// BigAncestor walks from path (symlinks resolved) up to / and returns the first
// directory that has more entries than the route walker lists (listlimit.Max),
// with its count, or one that cannot be read, with the error. It returns "" for
// a path whose every ancestor is fine.
func BigAncestor(path string) (dir string, n int, err error) {
	for d := resolve(path); ; d = filepath.Dir(d) {
		n, err := countEntries(d)
		if err != nil {
			return d, 0, err
		}
		if n > listlimit.Max {
			return d, n, nil
		}
		if filepath.Dir(d) == d {
			return "", 0, nil
		}
	}
}

// insideCheckout reports whether a .git sits in dir or any parent.
func insideCheckout(dir string) bool {
	for d := resolve(dir); ; d = filepath.Dir(d) {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return true
		}
		if filepath.Dir(d) == d {
			return false
		}
	}
}

// pickBase returns the first candidate that is not inside a checkout, holds at
// most smallBase entries and has no ancestor (itself included) the route walker
// would refuse to list.
func pickBase(cands []string) (string, bool) {
	for _, c := range cands {
		if c == "" || insideCheckout(c) {
			continue
		}
		if n, err := countEntries(c); err != nil || n > smallBase {
			continue
		}
		if d, _, err := BigAncestor(c); d == "" && err == nil {
			return c, true
		}
	}
	return "", false
}

// SkipIfInvalid is the first line of every budget gate: when an ancestor of
// the fixture's TMPDIR (or, in prebuilt mode, of the temp HOME) has more than
// listlimit.Max entries or cannot be read, the route walker would refuse every
// fixture folder and the gate would measure nothing, so it is skipped as
// INVALID, never failed.
func SkipIfInvalid(t skipper) (dirs []string) {
	t.Helper()
	dirs = invalidDirs()
	CheckDirs(t, dirs)
	return dirs
}

// CheckDirs skips the gate as INVALID when an ancestor of any of dirs is too
// big or unreadable now. Gates call it with the dirs SkipIfInvalid returned at
// their start, before they judge their result, so INVALID wins over FAIL.
func CheckDirs(t skipper, dirs []string) (skipped bool) {
	t.Helper()
	if big, n, err := firstInvalid(dirs); big != "" || err != nil {
		t.Skipf("%s", invalidMsg(big, n, err))
		return true
	}
	return false
}

// Judge runs a gate's verdict (its floors and comparisons) only after the
// validity check on the start dirs passes, so INVALID always wins over FAIL.
func Judge(t skipper, dirs []string, verdict func()) {
	t.Helper()
	if CheckDirs(t, dirs) {
		return
	}
	verdict()
}

// SetCountEntriesForTest replaces the directory entry counter and returns the
// function that restores it, so a gate test can make an ancestor grow mid-run.
func SetCountEntriesForTest(f func(dir string) (int, error)) (restore func()) {
	old := countEntries
	countEntries = f
	return func() { countEntries = old }
}

// firstInvalid is the first too-big or unreadable ancestor of any invalidDirs
// entry, with its entry count or read error.
func firstInvalid(dirs []string) (dir string, n int, err error) {
	for _, d := range dirs {
		if big, n, err := BigAncestor(d); big != "" {
			return big, n, err
		}
	}
	return "", 0, nil
}

func invalidMsg(big string, n int, err error) string {
	if err != nil {
		return fmt.Sprintf("INVALID: %s cannot be read (%v); it is an ancestor of the budget fixture, and the route walker cannot resolve fixture folders under a directory it cannot list. Point TMPDIR at a directory whose ancestors all have at most %d entries", big, err, listlimit.Max)
	}
	return fmt.Sprintf("INVALID: %s has %d entries (more than %d); it is an ancestor of the budget fixture, and the route walker will not list it, so every fixture folder is denied. Point TMPDIR at a directory whose ancestors all have at most %d entries", big, n, listlimit.Max, listlimit.Max)
}
