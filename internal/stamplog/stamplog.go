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
	ReasonControl   = "control character in line"
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

func hasControl(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < 0x20 && c != '\t') || c == 0x7f {
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

// Append adds one entry to the log at path, creating it 0600 if it is new. It
// refuses (writing nothing) bad text, a path that is not a regular file, and a
// file whose last byte is not a newline. Writers are serialised by an advisory
// lock on the file, and the entry goes out in one write.
func Append(path, text string, now time.Time, opts Options) error {
	line, err := FormatEntry(now, text)
	if err != nil {
		return err
	}
	timeout := opts.LockTimeout
	if timeout <= 0 {
		timeout = DefaultLockTimeout
	}
	// O_NONBLOCK keeps a FIFO from blocking the open; it is harmless on a regular file.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE|nonblock, 0600)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrFile, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrFile, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", ErrFile, path)
	}
	release, err := lock.WaitFile(f, timeout)
	if err != nil {
		return fmt.Errorf("log %s: %w", path, err)
	}
	defer release()
	// Size is read under the lock, so a writer that was mid-line is seen.
	if fi, err = f.Stat(); err != nil {
		return fmt.Errorf("%w: %v", ErrFile, err)
	}
	if n := fi.Size(); n > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], n-1); err != nil {
			return fmt.Errorf("%w: %v", ErrFile, err)
		}
		if last[0] != '\n' {
			return fmt.Errorf("%w: %s does not end with a newline; the new entry would join a broken line (run: claude-memsync log check %s)", ErrFile, path, path)
		}
	}
	if _, err := f.WriteString(line); err != nil {
		return fmt.Errorf("log %s: %w", path, err)
	}
	return nil
}

// Problem is one line Check rejects. Line is 1-based.
type Problem struct {
	Line   int
	Reason string
}

// Check reads a log and reports every line a prefix filter would skip or a
// reader would choke on. Blank lines and lines starting with "#" are fine.
func Check(r io.Reader) []Problem {
	var out []Problem
	br := bufio.NewReader(r)
	for n := 1; ; n++ {
		raw, err := br.ReadString('\n')
		if raw == "" && err != nil {
			break
		}
		complete := strings.HasSuffix(raw, "\n")
		if reason := checkLine(strings.TrimSuffix(raw, "\n"), len(raw)); reason != "" {
			out = append(out, Problem{n, reason})
		} else if !complete {
			out = append(out, Problem{n, ReasonNoNewline})
		}
		if err != nil {
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
