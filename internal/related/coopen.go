package related

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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

// Session is one Claude transcript file, reduced to when it ran.
// Nothing of what was said is kept.
type Session struct {
	Start, End time.Time
}

// transcriptLine is the only part of a transcript line that is ever decoded.
// Decoding into this struct drops every other field (message content included)
// as the parser passes it.
type transcriptLine struct {
	Timestamp string `json:"timestamp"`
}

// ReadSessions reads the *.jsonl files directly under dir (one Claude project
// folder) and returns one Session per file that has a timestamp. It reads each
// line's timestamp and nothing else.
func ReadSessions(dir string) ([]Session, error) {
	if _, err := os.ReadDir(dir); err != nil {
		return nil, err
	}
	ix := &SessionIndex{}
	out, _ := ix.Update(dir, "", time.Time{})
	return out, nil
}

// openTranscript opens a transcript (a seam for tests).
var openTranscript = func(p string) (io.ReadSeekCloser, error) { return os.Open(p) }

// FileState is what is remembered of one transcript between runs: how far it
// was read and the times seen. No content.
type FileState struct {
	Mtime  int64
	Size   int64
	Offset int64 // bytes through the last complete line read
	Has    bool
	Start  time.Time
	End    time.Time
}

// scanCheckLines and scanCheckBytes say how often a scan looks at the clock.
const (
	scanCheckLines = 10000
	scanCheckBytes = 8 << 20
)

// nowFn is the clock (a seam for tests).
var nowFn = time.Now

// fileKey names a transcript in the index without a path in it: the project
// folder is the escaped checkout path, so only its hash is stored.
func fileKey(folder, name string) string {
	h := sha256.Sum256([]byte(folder))
	return hex.EncodeToString(h[:8]) + "/" + name
}

// SessionIndex makes reading transcripts incremental: a file whose size and
// mtime are unchanged is not opened, a grown one is read from its offset.
type SessionIndex struct {
	Files map[string]FileState
}

// LoadSessionIndex reads the index; a missing or unreadable one is empty.
func LoadSessionIndex(path string) *SessionIndex {
	ix := &SessionIndex{}
	if b, err := os.ReadFile(path); err == nil {
		if json.Unmarshal(b, ix) != nil {
			ix = &SessionIndex{}
		}
	}
	return ix
}

// Save writes the index.
func (ix *SessionIndex) Save(path string) error {
	b, err := json.Marshal(ix)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Update brings the files of one project folder up to date and returns its
// sessions. A zero deadline means no limit. complete is false when the
// deadline stopped it before every changed file was read (what was read is
// kept; the next run continues).
func (ix *SessionIndex) Update(dir, folder string, deadline time.Time) (out []Session, complete bool) {
	if ix.Files == nil {
		ix.Files = map[string]FileState{}
	}
	complete = true
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, true
	}
	live := map[string]bool{}
	for _, e := range ents {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		key := fileKey(folder, e.Name())
		live[key] = true
		info, err := e.Info()
		if err != nil {
			continue
		}
		st := ix.Files[key]
		if st.Size != info.Size() || st.Mtime != info.ModTime().UnixNano() {
			if !deadline.IsZero() && nowFn().After(deadline) {
				complete = false
			} else {
				var done bool
				st, done = readTranscript(filepath.Join(dir, e.Name()), st, info, deadline)
				ix.Files[key] = st
				if !done {
					complete = false
				}
			}
		}
		if st.Has {
			out = append(out, Session{Start: st.Start, End: st.End})
		}
	}
	prefix := fileKey(folder, "")
	for k := range ix.Files {
		if strings.HasPrefix(k, prefix) && !strings.Contains(k[len(prefix):], "/") && !live[k] {
			delete(ix.Files, k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, complete
}

// readTranscript reads the new lines of one transcript. done is false when the
// deadline stopped it: the offset is kept and the size and mtime are not, so
// the next run reads on from there.
func readTranscript(path string, st FileState, info os.FileInfo, deadline time.Time) (_ FileState, done bool) {
	if info.Size() < st.Offset {
		st = FileState{} // truncated or replaced: start over
	}
	f, err := openTranscript(path)
	if err != nil {
		return st, true
	}
	defer f.Close()
	if _, err := f.Seek(st.Offset, io.SeekStart); err != nil {
		return st, true
	}
	stopped := false
	stop := func() bool {
		if !deadline.IsZero() && nowFn().After(deadline) {
			stopped = true
		}
		return stopped
	}
	consumed, _ := scanLines(f, maxTranscriptLine, func(b []byte) {
		var l transcriptLine
		if json.Unmarshal(b, &l) != nil || l.Timestamp == "" {
			return
		}
		ts, err := time.Parse(time.RFC3339Nano, l.Timestamp)
		if err != nil {
			return
		}
		ts = ts.UTC()
		if !st.Has {
			st.Start, st.End, st.Has = ts, ts, true
		}
		if ts.Before(st.Start) {
			st.Start = ts
		}
		if ts.After(st.End) {
			st.End = ts
		}
	}, stop)
	st.Offset += consumed
	if stopped {
		return st, false
	}
	st.Size, st.Mtime = info.Size(), info.ModTime().UnixNano()
	return st, true
}

// scanLines calls fn for every line of r that is at most max bytes long; longer
// lines are skipped without being kept. fn must not retain its argument. A
// last line without a newline is passed to fn too (it may still be growing) but
// is not counted in consumed, which is the byte count through the last newline.
// stop (may be nil) is asked every scanCheckLines lines or scanCheckBytes bytes;
// when it says true the scan ends at that line boundary.
func scanLines(r io.Reader, max int, fn func([]byte), stop func() bool) (consumed int64, err error) {
	br := bufio.NewReaderSize(r, 64<<10)
	var buf []byte
	over := false
	var lineLen, sinceCheck int64
	lines := 0
	for {
		chunk, rerr := br.ReadSlice('\n')
		lineLen += int64(len(chunk))
		if !over {
			buf = append(buf, chunk...)
			if len(buf) > max+2 {
				over, buf = true, nil
			}
		}
		if rerr == bufio.ErrBufferFull {
			continue
		}
		complete := rerr == nil
		if !over {
			line := bytes.TrimRight(buf, "\r\n")
			if len(line) > 0 && len(line) <= max {
				fn(line)
			}
		}
		if complete {
			consumed += lineLen
			sinceCheck += lineLen
			lines++
			if stop != nil && (lines >= scanCheckLines || sinceCheck >= scanCheckBytes) {
				lines, sinceCheck = 0, 0
				if stop() {
					return consumed, nil
				}
			}
		}
		buf, over, lineLen = buf[:0], false, 0
		if rerr != nil {
			if rerr == io.EOF {
				return consumed, nil
			}
			return consumed, rerr
		}
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
