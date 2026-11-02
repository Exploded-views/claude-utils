package main

import "io"

// unitVersionID is the stable id of the doctor check that the pieces naming
// the running build agree.
const unitVersionID = "unit-version"

// unitPiece is one place that says which build runs.
type unitPiece struct {
	Name string // "Condition", "ExecStart" or "symlink"
	Path string // what it points at, as written
}

// checkResult is the outcome of one doctor check.
type checkResult struct {
	ID     string
	OK     bool
	Detail string
	Pieces []unitPiece
}

// binLinkDirFn is the directory holding the claude-memsync bin/ symlinks;
// tests replace it.
var binLinkDirFn = func() (string, error) { return "", nil }

func unitVersionCheck() checkResult { return checkResult{ID: unitVersionID, OK: true} }

func reportUnitVersion(w io.Writer, r checkResult) bool { return r.OK }
