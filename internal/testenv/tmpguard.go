package testenv

import "os"

// tempCandidates are the bases SmallTempDir may use, in order.
var tempCandidates = func() []string { return []string{os.TempDir(), "/tmp", "/var/tmp"} }

// countEntries is the number of entries in dir.
var countEntries = func(dir string) (int, error) { return 0, nil }

// invalidDirs are the directories whose ancestors the budget gates need small.
var invalidDirs = func() []string { return nil }

// skipper is the part of testing.TB that SkipIfInvalid uses.
type skipper interface {
	Helper()
	Skipf(format string, args ...any)
}

// pickBase returns the first candidate that qualifies.
func pickBase(cands []string) (string, bool) { return "", false }

// BigAncestor returns the first directory, from path up to /, that has too
// many entries or cannot be read.
func BigAncestor(path string) (dir string, n int, err error) { return "", 0, nil }

// SkipIfInvalid skips the gate as INVALID when an ancestor of its fixture is too big.
func SkipIfInvalid(t skipper) {}
