package testenv

import (
	"io"
	"os"
	"path/filepath"

	"github.com/MarimerLLC/claude-utils/internal/listlimit"
)

// smallBase is the most entries the base dir itself may hold: a fixture made
// directly under a busier one would be slow to list for no reason.
const smallBase = 200

// tempCandidates are the bases SmallTempDir may use, in order: the temp dir the
// process started with, then /tmp, then /var/tmp. A test replaces it.
var tempCandidates = func() []string { return []string{os.TempDir(), "/tmp", "/var/tmp"} }

// countEntries is the number of entries in dir. A test replaces it, so no
// check depends on the host's real directories.
var countEntries = func(dir string) (int, error) {
	f, err := os.Open(dir)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
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
func SkipIfInvalid(t skipper) {
	t.Helper()
	for _, d := range invalidDirs() {
		big, n, err := BigAncestor(d)
		switch {
		case err != nil:
			t.Skipf("INVALID: %s cannot be read (%v); it is an ancestor of %s, and the route walker cannot resolve fixture folders under a directory it cannot list. Point TMPDIR at a directory whose ancestors all have at most %d entries", big, err, d, listlimit.Max)
		case big != "":
			t.Skipf("INVALID: %s has %d entries (more than %d); it is an ancestor of %s, and the route walker will not list it, so every fixture folder is denied. Point TMPDIR at a directory whose ancestors all have at most %d entries", big, n, listlimit.Max, d, listlimit.Max)
		}
	}
}
