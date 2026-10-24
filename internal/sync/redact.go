package sync

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
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
	// foldCase: the file system ignores case, so a root is matched without it.
	foldCase bool
}

// rootPrefix maps an absolute directory to the sync-repo-relative form of the
// paths under it, which is what denylist patterns are written against.
type rootPrefix struct{ abs, rel string }

// foldsCase reports whether paths on goos are compared without regard to case.
func foldsCase(goos string) bool { return goos == "windows" || goos == "darwin" }

// asciiLower is s with A-Z folded, byte for byte: it keeps the length, which
// strings.ToLower does not for every rune, so offsets in it are offsets in s.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

const redactedPath = "<path redacted>"

// newPathGuard loads the denylist. A missing file means nothing to hide.
func newPathGuard(cfg config.Config) *pathGuard {
	g := &pathGuard{foldCase: foldsCase(runtime.GOOS), roots: []rootPrefix{
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
	if g != nil && !g.closed && (g.scanner == nil || s == "") {
		return s, false
	}
	var b strings.Builder
	hit := false
	for _, line := range strings.SplitAfter(s, "\n") {
		out, ok := g.redactLine(line)
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

// maxLineBytes is the longest line that is searched. A longer one is redacted
// whole, since it cannot be vouched for within a sane amount of work.
const maxLineBytes = 64 << 10

type span struct{ start, end int }

// redactLine replaces the hidden names of one line (a trailing newline is
// kept). The work is linear in the length of the line for each denylist
// pattern. A name can be written many ways in an error, so the line is searched
// in four ways, and the matches are widened to whole words and joined:
//
//   - each pattern is run over the line as it is;
//   - and over a normalised copy of it, with git's pathspec magic such as
//     ":(literal)" and the quotes cut out, backslashes turned to slashes and
//     the absolute sync and Claude roots written as the repo-relative paths
//     patterns are written against;
//   - each word of the copy (split on white space and quotes, then on colons)
//     is tested on its own, so an anchored pattern matches a path token;
//   - the line as a whole, trimmed, is tested if nothing in it matched.
//
// Patterns are run with their anchors relaxed to name boundaries (see
// scrub.Scanner.PathSpans), which finds an anchored name with spaces in the
// middle of a line. A line over maxLineBytes is redacted whole.
func (g *pathGuard) redactLine(line string) (string, bool) {
	body := strings.TrimRight(line, "\r\n")
	tail := line[len(body):]
	if strings.TrimSpace(body) == "" {
		return line, false
	}
	if len(body) > maxLineBytes {
		return redactedPath + tail, true
	}
	var marks []span
	if g == nil || g.closed {
		marks = closedMarks(body)
	} else {
		marks = widen(body, g.scannerMarks(body))
	}
	if len(marks) == 0 {
		return line, false
	}
	return apply(body, marks) + tail, true
}

// closedMarks is the words and quoted strings of body that look like paths, for
// a guard with no denylist to ask.
func closedMarks(body string) []span {
	words := splitWords(body)
	var marks []span
	marked := make([]bool, len(words))
	for i, w := range words {
		if sp, ok := hiddenPart(body, w, looksLikePath); ok {
			marks = append(marks, sp)
			marked[i] = true
		}
	}
	return append(marks, hiddenQuotes(body, words, marked, looksLikePath)...)
}

// scannerMarks is the parts of body that the denylist matches, found in the
// ways listed at redactLine.
func (g *pathGuard) scannerMarks(body string) []span {
	var marks []span
	for _, m := range g.scanner.PathSpans(body) {
		marks = append(marks, span{m[0], m[1]})
	}
	norm := g.normalise(body)
	if norm.text != body {
		for _, m := range g.scanner.PathSpans(norm.text) {
			marks = append(marks, norm.back(span{m[0], m[1]}))
		}
	}
	hide := func(tok string) bool { return g.scanner.MatchesPath(tok) }
	for _, w := range splitWords(norm.text) {
		if sp, ok := hiddenPart(norm.text, w, hide); ok {
			marks = append(marks, norm.back(sp))
		}
	}
	// Whole, if nothing in it matched: an unanchored pattern matches any line
	// that holds a hit, and that line is already marked.
	if trimmed := strings.TrimSpace(norm.text); len(marks) == 0 && trimmed != "" && hide(trimmed) {
		start := strings.Index(norm.text, trimmed)
		marks = append(marks, norm.back(span{start, start + len(trimmed)}))
	}
	return marks
}

// normalised is a copy of a line with an offset map back to the original.
type normalised struct {
	text       string
	start, end []int // per byte of text: the range of the original it came from
}

// back maps a span of the copy to the span of the original it came from.
func (n *normalised) back(sp span) span {
	return span{n.start[sp.start], n.end[sp.end-1]}
}

// normalise cuts the pathspec magic and the quotes out of body, turns
// backslashes into slashes, and writes the absolute sync and Claude roots as
// the repo-relative paths under them.
func (g *pathGuard) normalise(body string) *normalised {
	type rep struct {
		start, end int
		text       string
	}
	var reps []rep
	// Go and git print a Windows path with backslashes and the roots are
	// slashed, so they are looked for in a copy of the line with its backslashes
	// turned (and its case folded, where the file system does): the copy is as
	// long as the line, and an offset in one is an offset in the other.
	hay := strings.ReplaceAll(body, `\`, "/")
	if g.foldCase {
		hay = asciiLower(hay)
	}
	for _, r := range g.roots {
		if r.abs == "" {
			continue
		}
		prefix := strings.ReplaceAll(r.abs, `\`, "/") + "/"
		if g.foldCase {
			prefix = asciiLower(prefix)
		}
		replen := len(prefix)
		text := ""
		if r.rel != "" {
			text = r.rel + "/"
		}
		for off := 0; ; {
			i := strings.Index(hay[off:], prefix)
			if i < 0 {
				break
			}
			reps = append(reps, rep{off + i, off + i + replen, text})
			off += i + replen
		}
	}
	sort.Slice(reps, func(i, j int) bool { return reps[i].start < reps[j].start })

	n := &normalised{}
	var b strings.Builder
	emit := func(c byte, from, to int) {
		b.WriteByte(c)
		n.start = append(n.start, from)
		n.end = append(n.end, to)
	}
	next := 0 // index into reps
	for i := 0; i < len(body); {
		for next < len(reps) && reps[next].start < i {
			next++ // overlaps what was just written
		}
		switch c := body[i]; {
		case next < len(reps) && reps[next].start == i:
			r := reps[next]
			for k := 0; k < len(r.text); k++ {
				emit(r.text[k], r.start, r.end)
			}
			i = r.end
		case c == '\'' || c == '"' || c == '`':
			i++
		case c == ':' && i+1 < len(body) && body[i+1] == '(':
			if loc := magicPrefix.FindStringIndex(body[i+1:]); loc != nil {
				i += 1 + loc[1]
				continue
			}
			emit(c, i, i+1)
			i++
		case c == '\\':
			emit('/', i, i+1)
			i++
		default:
			emit(c, i, i+1)
			i++
		}
	}
	n.text = b.String()
	return n
}

// widen extends each span to the whole words it touches (a word ends at white
// space or a quote), without the punctuation that only wraps it, and drops the
// spans that nothing is left of.
func widen(body string, marks []span) []span {
	out := make([]span, 0, len(marks))
	for _, m := range marks {
		// A relaxed anchor takes the delimiter beside the name along; it is not
		// part of the name.
		ms, me := m.start, m.end
		for ms < me && (isBreak(body[ms]) || strings.IndexByte(punct, body[ms]) >= 0) {
			ms++
		}
		for me > ms && (isBreak(body[me-1]) || strings.IndexByte(punct, body[me-1]) >= 0) {
			me--
		}
		if ms >= me {
			continue
		}
		s, e := ms, me
		for s > 0 && !isBreak(body[s-1]) {
			s--
		}
		for e < len(body) && !isBreak(body[e]) {
			e++
		}
		for s < ms && strings.IndexByte(punct, body[s]) >= 0 {
			s++
		}
		for e > me && (strings.IndexByte(punct, body[e-1]) >= 0 || body[e-1] == '.') {
			e--
		}
		out = append(out, span{s, e})
	}
	return out
}

func isBreak(c byte) bool {
	switch c {
	case ' ', '\t', '\r', '\n', '\'', '"', '`':
		return true
	}
	return false
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
