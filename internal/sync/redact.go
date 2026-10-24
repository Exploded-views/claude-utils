package sync

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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
// A path that matches the denylist is never printed: each token that matches
// is replaced by a placeholder, and the text around it (the exit status, git's
// reason) is kept. If the denylist exists but cannot be parsed, no pattern can
// be ruled out, so every path-like token is replaced.
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
// names a path that must not be logged. Only the tokens that name such a path
// are replaced: the wrapping text, the exit status and git's reason stay, so
// the error is still diagnosable.
func (g *pathGuard) Err(err error) error {
	if err == nil {
		return nil
	}
	msg, hit := g.redact(err.Error())
	if !hit {
		return err
	}
	return &redactedError{msg: msg, err: err}
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
	_, hit := g.redact(s)
	return hit
}

// redact returns s with every token that names a hidden path replaced by
// redactedPath, and whether any was. A nil guard, or one whose denylist could
// not be read, hides every path-like token.
func (g *pathGuard) redact(s string) (string, bool) {
	hide := g.hidesToken
	if g == nil || g.closed {
		hide = looksLikePath
	} else if g.scanner == nil || s == "" {
		return s, false
	}
	var b strings.Builder
	hit := false
	for _, line := range strings.SplitAfter(s, "\n") {
		out, ok := redactLine(line, hide)
		b.WriteString(out)
		hit = hit || ok
	}
	if !hit {
		return s, false
	}
	return b.String(), true
}

// looksLikePath is the test when no denylist can vouch for a name: anything
// with a separator or a file extension.
func looksLikePath(tok string) bool {
	if strings.ContainsAny(tok, `/\`) {
		return true
	}
	i := strings.LastIndexByte(tok, '.')
	return i > 0 && i < len(tok)-1
}

// hidesToken reports whether tok matches the denylist, as a repo-relative path
// or under one of the root directories.
func (g *pathGuard) hidesToken(tok string) bool {
	return g.matches(filepath.ToSlash(tok))
}

// maxRunWords bounds a run of words tried as one name that holds spaces, on a
// line longer than maxFullLineChars. maxFullLineChars is the line length, in
// characters, up to which every run of words is tried, of any word count.
const (
	maxRunWords      = 12
	maxFullLineChars = 1024
)

type span struct{ start, end int }

// redactLine replaces the hidden tokens of one line (a trailing newline is
// kept). The line is searched in four ways, since a name can be written many
// ways in an error:
//
//   - each word, split on whitespace and quotes, then on colons, with a git
//     magic prefix such as "(literal)" cut off, so ":(literal)projects/x" and
//     "projects/x:" are seen as "projects/x";
//   - each quoted string, for a name with spaces in quotes;
//   - runs of words, for a name with spaces that is not quoted: every run, of
//     any word count, on a line of up to maxFullLineChars characters (which
//     includes the rest of the line from each word), and only runs of up to
//     maxRunWords words on a longer one, to bound the work;
//   - the line as a whole, if nothing in it matched.
//
// Matches in a word cover only the matching part of it.
func redactLine(line string, hide func(string) bool) (string, bool) {
	body := strings.TrimRight(line, "\r\n")
	tail := line[len(body):]
	words := splitWords(body)
	var marks []span
	marked := make([]bool, len(words))
	for i, w := range words {
		if sp, ok := hiddenPart(body, w, hide); ok {
			marks = append(marks, sp)
			marked[i] = true
		}
	}
	marks = append(marks, hiddenQuotes(body, words, marked, hide)...)
	// Every run of words on a line up to maxFullLineChars; beyond that the
	// work is bounded by a window of maxRunWords words.
	window := len(words)
	if len([]rune(body)) > maxFullLineChars {
		window = maxRunWords
	}
	for i := 0; i < len(words); i++ {
		if marked[i] {
			continue
		}
		for j := i + 1; j < len(words) && j < i+window && !marked[j]; j++ {
			run := span{words[i].start, words[j].end}
			if hide(trimPunct(body[run.start:run.end])) {
				marks = append(marks, run)
				i = j
				break
			}
		}
	}
	if len(marks) == 0 {
		trimmed := strings.TrimSpace(body)
		if trimmed == "" || !hide(trimmed) {
			return line, false
		}
		start := strings.Index(body, trimmed)
		marks = append(marks, span{start, start + len(trimmed)})
	}
	return apply(body, marks) + tail, true
}

// splitWords returns the runs of body that hold no whitespace and no quote.
func splitWords(body string) []span {
	var words []span
	start := -1
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case ' ', '\t', '\r', '\'', '"', '`':
			if start >= 0 {
				words = append(words, span{start, i})
				start = -1
			}
		default:
			if start < 0 {
				start = i
			}
		}
	}
	if start >= 0 {
		words = append(words, span{start, len(body)})
	}
	return words
}

// hiddenQuotes tests each quoted string that holds a space, unless one of its
// words already matched (an unanchored pattern would then match all of it).
func hiddenQuotes(body string, words []span, marked []bool, hide func(string) bool) []span {
	var marks []span
	for i := 0; i < len(body); i++ {
		q := body[i]
		if q != '\'' && q != '"' && q != '`' {
			continue
		}
		end := strings.IndexByte(body[i+1:], q)
		if end < 0 {
			break
		}
		sp := span{i + 1, i + 1 + end}
		i = sp.end
		inner := body[sp.start:sp.end]
		if !strings.ContainsAny(inner, " \t") {
			continue
		}
		clean := true
		for k, w := range words {
			if marked[k] && w.start >= sp.start && w.end <= sp.end {
				clean = false
			}
		}
		if clean && hide(trimPunct(inner)) {
			marks = append(marks, sp)
		}
	}
	return marks
}

// hiddenPart finds the part of word w to hide: the whole word, else one of its
// colon-separated parts, with a magic prefix like "(literal)" cut off.
func hiddenPart(body string, w span, hide func(string) bool) (span, bool) {
	if sp, ok := trimmedHidden(body, w, hide); ok {
		return sp, true
	}
	start := w.start
	for i := w.start; i <= w.end; i++ {
		if i < w.end && body[i] != ':' {
			continue
		}
		if i > start {
			if sp, ok := trimmedHidden(body, span{start, i}, hide); ok {
				return sp, true
			}
		}
		start = i + 1
	}
	return span{}, false
}

// magicPrefix is a git pathspec magic group such as "(literal)" or
// "(literal,icase)".
var magicPrefix = regexp.MustCompile(`^\([A-Za-z,]+\)`)

// trimmedHidden tests sp with surrounding punctuation cut off, then again
// with a magic prefix cut off, and returns the span that was tested.
func trimmedHidden(body string, sp span, hide func(string) bool) (span, bool) {
	if got, ok := trimmedTest(body, sp, hide); ok {
		return got, true
	}
	// ":(literal)path": the prefix follows the colon, which trimming would
	// cut together with the parenthesis.
	for sp.start < sp.end && body[sp.start] == ':' {
		sp.start++
	}
	if loc := magicPrefix.FindStringIndex(body[sp.start:sp.end]); loc != nil {
		return trimmedTest(body, span{sp.start + loc[1], sp.end}, hide)
	}
	return span{}, false
}

func trimmedTest(body string, sp span, hide func(string) bool) (span, bool) {
	for sp.start < sp.end && strings.IndexByte(punct, body[sp.start]) >= 0 {
		sp.start++
	}
	for sp.end > sp.start && strings.IndexByte(punct, body[sp.end-1]) >= 0 {
		sp.end--
	}
	if sp.start >= sp.end {
		return span{}, false
	}
	if hide(body[sp.start:sp.end]) {
		return sp, true
	}
	// A trailing period ends a sentence, not a name.
	if body[sp.end-1] == '.' && sp.end-1 > sp.start && hide(body[sp.start:sp.end-1]) {
		return span{sp.start, sp.end - 1}, true
	}
	return span{}, false
}

const punct = "()[]{},;:"

func trimPunct(s string) string { return strings.Trim(s, punct) }

// apply replaces each marked span of body with redactedPath. Overlapping spans
// are merged.
func apply(body string, marks []span) string {
	sort.Slice(marks, func(i, j int) bool { return marks[i].start < marks[j].start })
	var b strings.Builder
	pos := 0
	for _, m := range marks {
		if m.start < pos {
			if m.end > pos {
				pos = m.end // overlaps the previous mark: it already covered the start
			}
			continue
		}
		b.WriteString(body[pos:m.start])
		b.WriteString(redactedPath)
		pos = m.end
	}
	b.WriteString(body[pos:])
	return b.String()
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
