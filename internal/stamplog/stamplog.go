// Package stamplog writes and checks UTC-minute-stamped decision and state
// logs. An entry is one line, "- YYYY-MM-DD HH:MMZ <text>"; the "- " and the
// stamp are the fixed prefix that readers filter on, so a line that lacks it
// is silently skipped by them. Append writes entries safely under concurrent
// writers; Check finds the lines a prefix filter would drop.
package stamplog

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/MarimerLLC/claude-utils/internal/lock"
)

// MaxLine is the longest entry line, newline included.
const MaxLine = 4096

// DefaultLockTimeout is how long Append waits for another writer.
const DefaultLockTimeout = 5 * time.Second

const stampLayout = "2006-01-02 15:04Z"

// The reasons Check reports.
const (
	ReasonPrefix    = `not a log entry: want "- YYYY-MM-DD HH:MMZ <text>", a "#" heading or a blank line`
	ReasonStamp     = "invalid date or time in the stamp"
	ReasonEmptyText = "entry has no text"
	ReasonControl   = "control, invisible or invalid-UTF-8 character in line"
	ReasonTooLong   = "line longer than 4096 bytes"
	ReasonConflict  = "merge conflict marker"
	ReasonNoNewline = "last line has no final newline"
)

// ErrText marks text that cannot be a log entry (usage error).
var ErrText = errors.New("bad log text")

// ErrFile marks a file that cannot safely be appended to.
var ErrFile = errors.New("log file not usable")

// Options tunes Append.
type Options struct {
	// LockTimeout is how long to wait for another writer; 0 means DefaultLockTimeout.
	LockTimeout time.Duration
}

// Stamp formats t in UTC, truncated (never rounded) to the minute.
func Stamp(t time.Time) string { return t.UTC().Truncate(time.Minute).Format(stampLayout) }

// hasControl reports text that must not appear in a log line: invalid UTF-8,
// C0 and C1 controls other than tab, DEL, line and paragraph separators, and
// format characters (bidi overrides and isolates, zero-width, BOM). They either
// break the one-line rule or make the bytes display as something else.
func hasControl(s string) bool {
	if !utf8.ValidString(s) {
		return true
	}
	for _, r := range s {
		if r == '\t' {
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
			return true
		}
	}
	return false
}

// FormatEntry returns the full entry line for text, newline included.
func FormatEntry(t time.Time, text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("%w: text is empty", ErrText)
	}
	if hasControl(text) {
		return "", fmt.Errorf("%w: text holds a newline or another control character (tab is allowed)", ErrText)
	}
	line := "- " + Stamp(t) + " " + text + "\n"
	if len(line) > MaxLine {
		return "", fmt.Errorf("%w: entry is %d bytes, the limit is %d", ErrText, len(line), MaxLine)
	}
	return line, nil
}

// Seams for tests.
var (
	sameFileHook = os.SameFile
	writeEntry   = func(f *os.File, s string) (int, error) { return f.WriteString(s) }
	waitFile     = lock.WaitFile
)

// maxOpenTries bounds the retries when the file is replaced while we wait.
const maxOpenTries = 5

// Append adds one entry to the log at path, creating it 0600 if it is new (a
// dangling symlink creates its target). It refuses (writing nothing) bad text,
// a path that is not a regular file, and a file whose last byte is not a
// newline. Concurrent Append calls are serialised by a lock on the file (an
// advisory flock on unix, so only other lockers respect it; a byte-range lock
// past the data on Windows), and the entry goes out in one write. The lock does
// not stop other tools (git, the sync mirror, an editor) rewriting the file;
// if the file is replaced by a rename while Append waits, it reopens the new
// file, a bounded number of times.
func Append(path, text string, now time.Time, opts Options) error {
	line, err := FormatEntry(now, text)
	if err != nil {
		return err
	}
	timeout := opts.LockTimeout
	if timeout <= 0 {
		timeout = DefaultLockTimeout
	}
	for i := 0; i < maxOpenTries; i++ {
		retry, err := appendOnce(path, line, timeout)
		if !retry {
			return err
		}
	}
	return fmt.Errorf("%w: %s was replaced %d times while waiting for the lock", ErrFile, path, maxOpenTries)
}

func appendOnce(path, line string, timeout time.Duration) (retry bool, err error) {
	// O_NONBLOCK keeps a FIFO from blocking the open; it is harmless on a regular file.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE|nonblock, 0600)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrFile, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrFile, err)
	}
	if !fi.Mode().IsRegular() {
		return false, fmt.Errorf("%w: %s is not a regular file", ErrFile, path)
	}
	release, err := waitFile(f, timeout)
	if err != nil {
		return false, fmt.Errorf("log %s: %w", path, err)
	}
	defer release()
	// The path may now name another file (replaced by a rename while we waited).
	if cur, err := os.Stat(path); err != nil || !sameFileHook(cur, fi) {
		return true, nil
	}
	// Size is read under the lock, so a writer that was mid-line is seen.
	if fi, err = f.Stat(); err != nil {
		return false, fmt.Errorf("%w: %v", ErrFile, err)
	}
	n := fi.Size()
	if n > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], n-1); err != nil {
			return false, fmt.Errorf("%w: %v", ErrFile, err)
		}
		if last[0] != '\n' {
			return false, fmt.Errorf("%w: %s does not end with a newline; the new entry would join a broken line (run: claude-memsync log check %s)", ErrFile, path, path)
		}
	}
	if _, err := writeEntry(f, line); err != nil {
		// Drop a torn fragment, or every later append would be refused.
		if terr := f.Truncate(n); terr != nil {
			return false, fmt.Errorf("log %s: %w (and truncating the fragment failed: %v)", path, err, terr)
		}
		return false, fmt.Errorf("log %s: %w", path, err)
	}
	return false, nil
}

// Problem is one line Check rejects. Line is 1-based.
type Problem struct {
	Line   int
	Reason string
}

// Check reads a log and reports every line a prefix filter would skip or a
// reader would choke on. Blank lines and lines starting with "#" are fine. A
// line is buffered only up to MaxLine+utf8.UTFMax bytes, however long it is.
func Check(r io.Reader) []Problem {
	var out []Problem
	br := bufio.NewReader(r)
	const keep = MaxLine + utf8.UTFMax
	for n := 1; ; n++ {
		var kept []byte
		total, complete, eof := 0, false, false
		for {
			chunk, err := br.ReadSlice('\n')
			total += len(chunk)
			if room := keep - len(kept); room > 0 {
				if len(chunk) > room {
					chunk = chunk[:room]
				}
				kept = append(kept, chunk...)
			}
			if err == nil {
				complete = true
				break
			}
			if err != bufio.ErrBufferFull {
				eof = true
				break
			}
		}
		if total == 0 {
			break
		}
		if total > MaxLine {
			// Cut at a rune boundary so the cut itself is not "invalid UTF-8".
			for i := 0; i < utf8.UTFMax && !utf8.Valid(kept); i++ {
				kept = kept[:len(kept)-1]
			}
		}
		if reason := checkLine(strings.TrimSuffix(string(kept), "\n"), total); reason != "" {
			out = append(out, Problem{n, reason})
		} else if !complete {
			out = append(out, Problem{n, ReasonNoNewline})
		}
		if eof {
			break
		}
	}
	return out
}

func checkLine(s string, rawLen int) string {
	if s == "" || s[0] == '#' {
		return ""
	}
	if hasControl(s) {
		return ReasonControl
	}
	if rawLen > MaxLine {
		return ReasonTooLong
	}
	for _, m := range []string{"<<<<<<<", "=======", ">>>>>>>"} {
		if strings.HasPrefix(s, m) {
			return ReasonConflict
		}
	}
	const stampEnd = 2 + len(stampLayout) // "- " + stamp
	if len(s) < stampEnd || !strings.HasPrefix(s, "- ") || !stampShape(s[2:stampEnd]) {
		return ReasonPrefix
	}
	if _, err := time.Parse(stampLayout, s[2:stampEnd]); err != nil {
		return ReasonStamp
	}
	rest := s[stampEnd:]
	if rest != "" && rest[0] != ' ' {
		return ReasonPrefix
	}
	if strings.TrimSpace(rest) == "" {
		return ReasonEmptyText
	}
	return ""
}

// stampShape reports whether s looks like "dddd-dd-dd dd:ddZ".
func stampShape(s string) bool {
	if len(s) != len(stampLayout) {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch i {
		case 4, 7:
			if s[i] != '-' {
				return false
			}
		case 10:
			if s[i] != ' ' {
				return false
			}
		case 13:
			if s[i] != ':' {
				return false
			}
		case 16:
			if s[i] != 'Z' {
				return false
			}
		default:
			if s[i] < '0' || s[i] > '9' {
				return false
			}
		}
	}
	return true
}
