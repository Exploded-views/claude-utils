package sync

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/scrub"
)

// pathGuard keeps a file name the user listed in the denylist out of the log.
// The name of a held-back file is as sensitive as its content, and error text
// is full of names: a PathError carries the absolute path, git's stderr names
// the files a pull or checkout tripped over. Everything the daemon logs or
// returns from a cycle on an error path goes through Err or Path.
//
// The rule is the simple one: a path that matches the denylist is never
// printed. If the denylist exists but cannot be parsed, no pattern can be
// ruled out, so no path is printed at all.
type pathGuard struct {
	scanner *scrub.Scanner // nil: no denylist, nothing to hide
	closed  bool           // denylist unreadable: hide every path
	roots   []rootPrefix
}

// rootPrefix maps an absolute directory to the sync-repo-relative form of the
// paths under it, which is what denylist patterns are written against.
type rootPrefix struct{ abs, rel string }

const redactedPath = "<path redacted>"

// newPathGuard loads the denylist. A missing file means nothing to hide.
func newPathGuard(cfg config.Config) *pathGuard {
	g := &pathGuard{roots: []rootPrefix{
		{filepath.ToSlash(cfg.SyncDir), ""},
		{filepath.ToSlash(cfg.ClaudeProjectsDir), "projects"},
	}}
	deny, err := loadDenylist(cfg)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		g.closed = true
	default:
		g.scanner = scrub.New(deny)
	}
	return g
}

// Path returns p, or a placeholder if p must not be logged. A nil guard hides
// everything.
func (g *pathGuard) Path(p string) string {
	if g.hides(p) {
		return redactedPath
	}
	return p
}

// Err returns err, or a stand-in with the same unwrap chain if its text
// names a path that must not be logged. Wrapping text before the offending
// part ("restore held-back files: ...") is kept.
func (g *pathGuard) Err(err error) error {
	if err == nil || !g.hides(err.Error()) {
		return err
	}
	switch u := err.(type) {
	case interface{ Unwrap() []error }:
		var parts []error
		for _, e := range u.Unwrap() {
			parts = append(parts, g.Err(e))
		}
		return &redactedError{msg: errors.Join(parts...).Error(), err: err}
	case interface{ Unwrap() error }:
		if inner := u.Unwrap(); inner != nil {
			msg := err.Error()
			if prefix, ok := strings.CutSuffix(msg, inner.Error()); ok && !g.hides(prefix) {
				return &redactedError{msg: prefix + g.Err(inner).Error(), err: err}
			}
		}
	}
	if errno, ok := fsErrno(err); ok {
		return &redactedError{msg: redactedPath + ": " + errno.Error(), err: err} // the errno, not the path
	}
	return &redactedError{msg: "<error text redacted: it names a path that matches the denylist>", err: err}
}

// fsErrno digs the underlying error out of a PathError or LinkError, whose
// own text is "op path: err".
func fsErrno(err error) (error, bool) {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err, true
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Err, true
	}
	return nil, false
}

// PathErr is an error for op on the repo-relative path rel, with underlying
// err's own paths (if it is a PathError or LinkError) dropped.
func (g *pathGuard) PathErr(op, rel string, err error) error {
	if errno, ok := fsErrno(err); ok {
		err = errno
	}
	return &redactedError{msg: op + " " + g.Path(rel) + ": " + err.Error(), err: err}
}

type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// hides reports whether s may name a path that must not be printed.
func (g *pathGuard) hides(s string) bool {
	if g == nil || g.closed {
		return strings.ContainsAny(s, `/\`)
	}
	if g.scanner == nil || s == "" {
		return false
	}
	// Whole text first: an unanchored pattern finds a name wherever it sits.
	// Then each run of words, with the root directories cut off: a pattern
	// anchored to the repo path ("^projects/...") only sees it that way, and a
	// name may hold spaces.
	s = filepath.ToSlash(s)
	if g.matches(s) {
		return true
	}
	words := strings.Fields(s)
	if len(words) > 40 {
		words = words[:40] // an error message, not a document
	}
	for i := range words {
		run := ""
		for j := i; j < len(words); j++ {
			if j > i {
				run += " "
			}
			run += words[j]
			if g.matches(strings.Trim(run, `'"():,`)) {
				return true
			}
		}
	}
	return false
}

func (g *pathGuard) matches(s string) bool {
	if g.scanner.MatchesPath(s) {
		return true
	}
	for _, r := range g.roots {
		if rest, ok := strings.CutPrefix(s, r.abs+"/"); ok && r.abs != "" {
			if r.rel != "" {
				rest = r.rel + "/" + rest
			}
			if g.scanner.MatchesPath(rest) {
				return true
			}
		}
	}
	return false
}
