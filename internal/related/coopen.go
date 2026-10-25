package related

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// maxTranscriptLine is the longest transcript line that is looked at. A longer
// line is skipped unread: transcripts hold whole tool results and file bodies,
// and only two small fields of each line are of any use here.
const maxTranscriptLine = 1 << 20

// Session is one Claude transcript file, reduced to where and when it ran.
// Nothing of what was said is kept.
type Session struct {
	Cwd        string
	Start, End time.Time
}

// transcriptLine is the only part of a transcript line that is ever decoded.
// Decoding into this struct drops every other field (message content included)
// as the parser passes it.
type transcriptLine struct {
	Cwd       string `json:"cwd"`
	Timestamp string `json:"timestamp"`
}

// ReadSessions reads the *.jsonl files directly under dir (one Claude project
// folder) and returns one Session per file that has a timestamp. It reads each
// line's cwd and timestamp and nothing else.
func ReadSessions(dir string) ([]Session, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Session
	for _, e := range ents {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		s, ok := readSession(filepath.Join(dir, e.Name()))
		if ok {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, nil
}

func readSession(path string) (Session, bool) {
	f, err := os.Open(path)
	if err != nil {
		return Session{}, false
	}
	defer f.Close()
	var s Session
	have := false
	_ = scanLines(f, maxTranscriptLine, func(b []byte) {
		var l transcriptLine
		if json.Unmarshal(b, &l) != nil || l.Timestamp == "" {
			return
		}
		ts, err := time.Parse(time.RFC3339Nano, l.Timestamp)
		if err != nil {
			return
		}
		ts = ts.UTC()
		if !have {
			s.Start, s.End, have = ts, ts, true
		}
		if ts.Before(s.Start) {
			s.Start = ts
		}
		if ts.After(s.End) {
			s.End = ts
		}
		if s.Cwd == "" {
			s.Cwd = l.Cwd
		}
	})
	return s, have
}

// scanLines calls fn for every line of r that is at most max bytes long; longer
// lines are skipped without being kept. fn must not retain its argument.
func scanLines(r io.Reader, max int, fn func([]byte)) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var buf []byte
	over := false
	for {
		chunk, prefix, err := br.ReadLine()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if !over {
			buf = append(buf, chunk...)
			if len(buf) > max {
				over, buf = true, nil
			}
		}
		if prefix {
			continue
		}
		if !over && len(buf) > 0 {
			fn(buf)
		}
		buf, over = buf[:0], false
	}
}

// coOpenDays counts the distinct UTC days on which a session of a and one of b
// overlapped in time or started within window of each other. The day is the
// one of the later start.
func coOpenDays(a, b []Session, window time.Duration) int {
	days := map[string]bool{}
	for _, x := range a {
		for _, y := range b {
			near := x.Start.Sub(y.Start)
			if near < 0 {
				near = -near
			}
			overlap := !x.Start.After(y.End) && !y.Start.After(x.End)
			if !overlap && near > window {
				continue
			}
			later := x.Start
			if y.Start.After(later) {
				later = y.Start
			}
			days[later.Format("2006-01-02")] = true
		}
	}
	return len(days)
}
