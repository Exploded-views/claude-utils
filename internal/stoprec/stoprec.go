// Package stoprec keeps small, private, bounded records of unclean service
// stops: what the service manager said (result, exit code, exit status) and a
// scrubbed tail of the journal, so a crash loop can be diagnosed later and
// pasted into a bug report.
//
// Records live in a directory of their own under the per-host config dir
// (Dir), which is never synced. The directory is 0700 and each record 0600. A
// record holds at most Limits.MaxLines tail lines of at most Limits.MaxLineBytes
// bytes and is at most Limits.MaxRecordBytes in all; the directory keeps at
// most Limits.MaxRecords records, evicting the oldest. Identical stops (same
// result, exit code and exit status) within Limits.Collapse of the first one
// merge into that record, with a repeat count, so a crash loop writes one
// record per hour, not one per restart.
//
// Nothing here talks to a service manager; callers hand in the text.
package stoprec

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/scrub"
)

// Limits bound a record, the directory, and what counts as a repeat.
type Limits struct {
	MaxLines       int           // tail lines kept per record (the newest)
	MaxLineBytes   int           // bytes kept per tail line
	MaxRecordBytes int           // size of one record file
	MaxRecords     int           // records kept; the oldest are evicted
	Collapse       time.Duration // identical stops this soon after the first merge into it
}

// Default is what record-stop uses.
var Default = Limits{MaxLines: 80, MaxLineBytes: 512, MaxRecordBytes: 16 << 10, MaxRecords: 20, Collapse: time.Hour}

// Note is written into every record. The scrub cannot see plain prose.
const Note = "Scrubbed tail: lines that look like secrets are withheld and denylisted paths are redacted, but prose in a note or an error message cannot be detected. A person or agent should review it before pasting, and paste only the lines needed, only to the fork."

// Record is one unclean stop, or a run of identical ones.
type Record struct {
	Note       string    `json:"note"`
	When       time.Time `json:"when"` // the first stop of the run
	Last       time.Time `json:"last"` // the latest
	Result     string    `json:"result"`
	ExitCode   string    `json:"exitCode,omitempty"`   // systemd's EXIT_CODE: exited, killed, dumped
	ExitStatus string    `json:"exitStatus,omitempty"` // EXIT_STATUS: a number or a signal name
	Version    string    `json:"version,omitempty"`
	Source     string    `json:"source,omitempty"`
	Tail       []string  `json:"tail"`
	Withheld   int       `json:"withheld,omitempty"` // tail lines the scrub held back
	Repeats    int       `json:"repeats"`
}

// Test seams.
var (
	renameFn   = os.Rename
	currentUID = os.Getuid
)

// Dir is where records live: the per-host config dir's "stops".
func Dir() (string, error) {
	d, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "stops"), nil
}

// EnsureDir makes dir a private directory this user owns: created 0700, a
// wider one we own tightened to 0700. A symlink, a non-directory and a
// directory owned by another user are refused.
func EnsureDir(dir string) error {
	st, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
			return err
		}
		if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		if err := os.Chmod(dir, 0o700); err != nil { // the umask may have narrowed it, never widened
			return err
		}
		st, err = os.Lstat(dir)
	}
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symbolic link; records are only written to a real directory", dir)
	}
	if !st.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if !ownedByMe(st) {
		return fmt.Errorf("%s belongs to another user", dir)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}

// checkDir is the read-side half of EnsureDir: it changes nothing. A missing
// directory is not an error (the caller sees no records).
func checkDir(dir string) error {
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
		return fmt.Errorf("%s is not a real directory", dir)
	}
	if !ownedByMe(st) {
		return fmt.Errorf("%s belongs to another user", dir)
	}
	return nil
}

var nameRE = regexp.MustCompile(`^stop-\d{8}T\d{6}Z-\d{2}\.json$`)

func recordName(t time.Time, seq int) string {
	return fmt.Sprintf("stop-%s-%02d.json", t.UTC().Format("20060102T150405Z"), seq)
}

type entry struct {
	name string
	rec  Record
}

// entries are the readable records of dir, newest first. A missing directory
// has none. Foreign files, symbolic links and corrupt records are skipped.
func entries(dir string) ([]entry, error) {
	if err := checkDir(dir); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		if e.Type().IsRegular() && nameRE.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	var out []entry
	for _, n := range names {
		f, err := os.Open(filepath.Join(dir, n))
		if err != nil {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(f, 64<<10))
		f.Close()
		var r Record
		if err != nil || json.Unmarshal(b, &r) != nil || r.Last.IsZero() {
			continue
		}
		r.Result, r.ExitCode, r.ExitStatus = field(r.Result), field(r.ExitCode), field(r.ExitStatus)
		r.Version, r.Source = clean(r.Version, 128), clean(r.Source, 64)
		out = append(out, entry{n, r})
	}
	// Newest stop first: by when it last happened, not when its record began.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].rec.Last.Equal(out[j].rec.Last) {
			return out[i].rec.Last.After(out[j].rec.Last)
		}
		return out[i].name > out[j].name
	})
	return out, nil
}

// List returns the records of dir, newest first.
func List(dir string) ([]Record, error) {
	es, err := entries(dir)
	recs := make([]Record, len(es))
	for i, e := range es {
		recs[i] = e.rec
	}
	return recs, err
}

// Latest is the newest record, if there is one.
func Latest(dir string) (Record, bool, error) {
	recs, err := List(dir)
	if err != nil || len(recs) == 0 {
		return Record{}, false, err
	}
	return recs[0], true, nil
}

func sameStop(a, b Record) bool {
	return a.Result == b.Result && a.ExitCode == b.ExitCode && a.ExitStatus == b.ExitStatus
}

// field limits a result, exit code or exit status to [A-Za-z0-9._-], anything
// else becoming "?", so free text (a token in an error string, a control
// sequence) can neither be stored nor printed. systemd's values are fixed
// words, numbers and signal names.
func field(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			return r
		}
		return '?'
	}, s)
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

var fieldScanner = scrub.New(nil)

// safeField is field, except that a value that trips a built-in scrub rule
// (a token is made of characters field allows) is stored as "withheld".
func safeField(s string) string {
	if firstBuiltin(fieldScanner.Scan([]byte(s))) != "" {
		return "withheld"
	}
	return field(s)
}

// clean makes s one printable line of at most max bytes, cut on a rune
// boundary.
func clean(s string, max int) string {
	s = strings.ToValidUTF8(s, "?")
	s = strings.Map(func(r rune) rune {
		if r == '\t' || unicode.IsControl(r) || r == ' ' || r == ' ' {
			return ' '
		}
		return r
	}, s)
	if len(s) > max {
		s = s[:max]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}

// Write stores rec. now is the time of the stop. It creates dir if needed
// (see EnsureDir), merges the stop into a recent identical one, keeps the
// record within lim and evicts the oldest records beyond lim.MaxRecords.
func Write(dir string, rec Record, now time.Time, lim Limits) error {
	if err := EnsureDir(dir); err != nil {
		return err
	}
	rec.Note = Note
	rec.Result, rec.ExitCode, rec.ExitStatus = safeField(rec.Result), safeField(rec.ExitCode), safeField(rec.ExitStatus)
	rec.Version = clean(rec.Version, 128)
	rec.Source = clean(rec.Source, 64)
	tail := rec.Tail
	if len(tail) > lim.MaxLines {
		tail = tail[len(tail)-lim.MaxLines:]
	}
	rec.Tail = make([]string, len(tail))
	for i, l := range tail {
		rec.Tail[i] = clean(l, lim.MaxLineBytes)
	}
	rec.When, rec.Last, rec.Repeats = now.UTC(), now.UTC(), 1

	es, err := entries(dir)
	if err != nil {
		return err
	}
	name := ""
	for _, e := range es {
		if age := now.Sub(e.rec.When); sameStop(e.rec, rec) && age >= 0 && age <= lim.Collapse {
			name, rec.When = e.name, e.rec.When
			rec.Repeats = max(e.rec.Repeats, 1) + 1
			break
		}
	}

	b, err := fit(&rec, lim.MaxRecordBytes)
	if err != nil {
		return err
	}
	if name == "" {
		if name, err = freeName(dir, now); err != nil {
			return err
		}
	}
	if err := writeAtomic(dir, name, b); err != nil {
		return err
	}
	return evict(dir, lim.MaxRecords, now)
}

// fit marshals rec, dropping the oldest tail lines until it is within max.
func fit(rec *Record, max int) ([]byte, error) {
	for {
		b, err := json.MarshalIndent(rec, "", " ")
		if err != nil {
			return nil, err
		}
		b = append(b, '\n')
		if len(b) <= max {
			return b, nil
		}
		if len(rec.Tail) == 0 {
			return nil, fmt.Errorf("a record cannot be made smaller than %d bytes", len(b))
		}
		// Drop in proportion to the excess, at least one line, so 80 lines
		// do not cost 80 marshals.
		drop := (len(b)-max)/(512+8) + 1
		if drop > len(rec.Tail) {
			drop = len(rec.Tail)
		}
		rec.Tail = rec.Tail[drop:]
	}
}

func freeName(dir string, now time.Time) (string, error) {
	for seq := 0; seq < 100; seq++ {
		n := recordName(now, seq)
		if _, err := os.Lstat(filepath.Join(dir, n)); errors.Is(err, os.ErrNotExist) {
			return n, nil
		}
	}
	return "", errors.New("too many records in one second")
}

// writeAtomic writes b to dir/name through a 0600 temp file in dir and a
// rename, so a reader never sees a partial record and a failure leaves none.
func writeAtomic(dir, name string, b []byte) (err error) {
	var rnd [6]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".tmp-"+hex.EncodeToString(rnd[:]))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|oNoFollow, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if err = f.Chmod(0o600); err != nil && !isWindowsChmod(err) {
		return err
	}
	err = nil
	if _, err = f.Write(b); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = renameFn(tmp, filepath.Join(dir, name)); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// staleTemp is how old a temp file must be before evict removes it: a writer
// that was killed between create and rename leaves one.
const staleTemp = time.Hour

func evict(dir string, keep int, now time.Time) error {
	es, err := entries(dir)
	if err != nil {
		return err
	}
	var first error
	if ents, err := os.ReadDir(dir); err == nil {
		for _, e := range ents {
			if !strings.HasPrefix(e.Name(), ".tmp-") || !e.Type().IsRegular() {
				continue
			}
			if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > staleTemp {
				os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	for i := keep; i < len(es); i++ {
		if err := os.Remove(filepath.Join(dir, es[i].name)); err != nil && first == nil {
			first = err
		}
	}
	return first
}
