package stamplog

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/lock"
)

var pdt = time.FixedZone("PDT", -7*3600)

func TestStamp_UTCFromInjectedClock(t *testing.T) {
	cases := []struct {
		name string
		in   time.Time
		want string
	}{
		{"pdt to utc", time.Date(2026, 11, 2, 11, 30, 0, 0, pdt), "2026-11-02 18:30Z"},
		{"seconds truncated not rounded", time.Date(2026, 11, 2, 11, 30, 59, 999, pdt), "2026-11-02 18:30Z"},
		{"utc midnight rolls the date", time.Date(2026, 11, 2, 20, 5, 0, 0, pdt), "2026-11-03 03:05Z"},
	}
	for _, c := range cases {
		if got := Stamp(c.in); got != c.want {
			t.Errorf("%s: Stamp = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestFormatEntry_ExactBytes(t *testing.T) {
	now := time.Date(2026, 11, 2, 18, 30, 5, 0, time.UTC)
	got, err := FormatEntry(now, "hello world")
	if err != nil {
		t.Fatal(err)
	}
	if want := "- 2026-11-02 18:30Z hello world\n"; got != want {
		t.Fatalf("FormatEntry = %q, want %q", got, want)
	}
	if got, err := FormatEntry(now, "a\tb"); err != nil || got != "- 2026-11-02 18:30Z a\tb\n" {
		t.Fatalf("tab text: %q, %v", got, err)
	}
}

func TestAppend_WritesAndAppends(t *testing.T) {
	p := filepath.Join(t.TempDir(), "log.md")
	now := time.Date(2026, 11, 2, 11, 30, 0, 0, pdt)
	for _, s := range []string{"one", "two"} {
		if err := Append(p, s, now, Options{}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := os.ReadFile(p)
	want := "- 2026-11-02 18:30Z one\n- 2026-11-02 18:30Z two\n"
	if string(got) != want {
		t.Fatalf("file = %q, want %q", got, want)
	}
}

func TestAppend_RefusesNewlineAndControl(t *testing.T) {
	now := time.Date(2026, 11, 2, 18, 30, 0, 0, time.UTC)
	texts := []string{"a\nb", "a\r", "a\x00", "\x1b[31m", "a\x7f", "", "   ", "\t", strings.Repeat("x", 4097)}
	for _, text := range texts {
		// A file that does not exist is not created.
		missing := filepath.Join(t.TempDir(), "new.md")
		err := Append(missing, text, now, Options{})
		if !errors.Is(err, ErrText) {
			t.Errorf("%q: err = %v, want ErrText", text, err)
		}
		if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
			t.Errorf("%q: file was created", text)
		}
		// An existing file is byte-identical afterwards.
		p := filepath.Join(t.TempDir(), "log.md")
		before := []byte("- 2026-11-02 18:29Z first\n")
		os.WriteFile(p, before, 0600)
		if err := Append(p, text, now, Options{}); !errors.Is(err, ErrText) {
			t.Errorf("%q: existing: err = %v, want ErrText", text, err)
		}
		if after, _ := os.ReadFile(p); !bytes.Equal(after, before) {
			t.Errorf("%q: file changed to %q", text, after)
		}
	}
}

func TestAppend_LineCapIs4096IncludingPrefix(t *testing.T) {
	now := time.Date(2026, 11, 2, 18, 30, 0, 0, time.UTC)
	prefix := len("- 2026-11-02 18:30Z ")
	p := filepath.Join(t.TempDir(), "log.md")
	ok := strings.Repeat("x", MaxLine-prefix-1) // plus the newline: exactly MaxLine bytes
	if err := Append(p, ok, now, Options{}); err != nil {
		t.Fatalf("a line of exactly %d bytes: %v", MaxLine, err)
	}
	if err := Append(p, ok+"x", now, Options{}); !errors.Is(err, ErrText) {
		t.Fatalf("one byte over: %v, want ErrText", err)
	}
	if probs := Check(strings.NewReader(mustRead(t, p))); len(probs) != 0 {
		t.Fatalf("Check on max line: %v", probs)
	}
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAppend_RefusesFileWithoutFinalNewline(t *testing.T) {
	p := filepath.Join(t.TempDir(), "log.md")
	before := []byte("- 2026-11-02 18:29Z broken")
	os.WriteFile(p, before, 0600)
	err := Append(p, "next", time.Now(), Options{})
	if !errors.Is(err, ErrFile) || errors.Is(err, ErrText) {
		t.Fatalf("err = %v, want ErrFile", err)
	}
	if !strings.Contains(err.Error(), "log check") {
		t.Errorf("message should point at log check: %v", err)
	}
	if after, _ := os.ReadFile(p); !bytes.Equal(after, before) {
		t.Fatalf("file changed: %q", after)
	}
}

func TestAppend_RefusesDirectory(t *testing.T) {
	if err := Append(t.TempDir(), "x", time.Now(), Options{}); err == nil {
		t.Fatal("appending to a directory succeeded")
	}
}

func TestAppend_LockTimeout(t *testing.T) {
	p := filepath.Join(t.TempDir(), "log.md")
	before := []byte("- 2026-11-02 18:29Z first\n")
	os.WriteFile(p, before, 0600)
	f, _ := os.OpenFile(p, os.O_RDWR, 0)
	defer f.Close()
	rel, err := lock.WaitFile(f, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	start := time.Now()
	err = Append(p, "x", time.Now(), Options{LockTimeout: 100 * time.Millisecond})
	if !errors.Is(err, lock.ErrTimeout) {
		t.Fatalf("err = %v, want lock.ErrTimeout", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("took far longer than the deadline")
	}
	if after, _ := os.ReadFile(p); !bytes.Equal(after, before) {
		t.Fatalf("file changed: %q", after)
	}
}

// The final-newline check must use the size read under the lock. A writer
// that dies mid-line while holding the lock leaves a fragment; an appender that
// opened the file before the fragment existed must still see it.
func TestAppend_WaitsForLockThenChecksFinalNewline(t *testing.T) {
	p := filepath.Join(t.TempDir(), "log.md")
	os.WriteFile(p, []byte("- 2026-11-02 18:29Z first\n"), 0600)
	f, _ := os.OpenFile(p, os.O_RDWR|os.O_APPEND, 0)
	defer f.Close()
	rel, err := lock.WaitFile(f, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- Append(p, "x", time.Now(), Options{LockTimeout: 5 * time.Second}) }()
	time.Sleep(200 * time.Millisecond) // the appender is now blocked on the lock
	select {
	case err := <-done:
		t.Fatalf("Append returned while the lock was held: %v", err)
	default:
	}
	if _, err := f.WriteString("- 2026-11-02 18:29Z partial"); err != nil {
		t.Fatal(err)
	}
	rel()
	err = <-done
	if !errors.Is(err, ErrFile) {
		t.Fatalf("err = %v, want ErrFile after waiting", err)
	}
	if time.Since(start) < 150*time.Millisecond {
		t.Fatal("did not wait for the lock")
	}
	if got := mustRead(t, p); got != "- 2026-11-02 18:29Z first\n- 2026-11-02 18:29Z partial" {
		t.Fatalf("file = %q", got)
	}
}

// The file replaced by a rename while an appender waits: the entry must land
// in the file now at the path, never in the orphaned inode.
func TestAppend_FileReplacedWhileWaiting(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "log.md")
	os.WriteFile(p, []byte("- 2026-11-02 18:29Z old\n"), 0600)
	f, _ := os.OpenFile(p, os.O_RDWR|os.O_APPEND, 0)
	defer f.Close()
	rel, err := lock.WaitFile(f, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- Append(p, "mine", time.Now(), Options{LockTimeout: 5 * time.Second}) }()
	time.Sleep(200 * time.Millisecond)
	repl := filepath.Join(dir, "repl")
	os.WriteFile(repl, []byte("- 2026-11-02 18:29Z replaced\n"), 0600)
	if err := os.Rename(repl, p); err != nil {
		t.Fatal(err)
	}
	rel()
	if err := <-done; err != nil {
		t.Fatalf("Append: %v", err)
	}
	got := mustRead(t, p)
	if !strings.HasPrefix(got, "- 2026-11-02 18:29Z replaced\n") || !strings.HasSuffix(got, "Z mine\n") {
		t.Fatalf("entry lost: file = %q", got)
	}
}

func TestFormatEntry_RefusesUnicodeControlAndInvisible(t *testing.T) {
	now := time.Date(2026, 11, 2, 18, 30, 0, 0, time.UTC)
	for name, text := range map[string]string{
		"C1 CSI":         "a\u009bb",
		"C1 NEL":         "a\u0085b",
		"line sep":       "a\u2028b",
		"para sep":       "a\u2029b",
		"bidi override":  "a\u202eb",
		"bidi isolate":   "a\u2066b",
		"zero width":     "a\u200bb",
		"BOM":            "\ufeffa",
		"invalid utf8":   "a\xffb",
		"truncated rune": "a\xe2\x82",
	} {
		if _, err := FormatEntry(now, text); !errors.Is(err, ErrText) {
			t.Errorf("%s: err = %v, want ErrText", name, err)
		}
	}
	for _, ok := range []string{"caf\u00e9 \u65e5\u672c \U0001F600", "tab\tok"} {
		if _, err := FormatEntry(now, ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
}

func TestCheck_FlagsUnicodeControlAndInvisible(t *testing.T) {
	for name, text := range map[string]string{
		"C1": "a\u009bb", "line sep": "a\u2028b", "bidi": "a\u202eb", "invalid utf8": "a\xffb",
	} {
		got := Check(strings.NewReader("- 2026-11-02 18:30Z " + text + "\n"))
		if len(got) != 1 || got[0].Reason != ReasonControl {
			t.Errorf("%s: %v, want ReasonControl", name, got)
		}
	}
}

func TestCheck_NoSpaceAfterStamp(t *testing.T) {
	got := Check(strings.NewReader("- 2026-11-02 18:30Zfoo\n"))
	if len(got) != 1 || got[0].Line != 1 || got[0].Reason != ReasonPrefix {
		t.Fatalf("got %v, want one ReasonPrefix on line 1", got)
	}
}

func TestCheck_HugeLineIsBoundedAndNumberingHolds(t *testing.T) {
	huge := "- 2026-11-02 18:30Z " + strings.Repeat("z", 3<<20) + "\n"
	got := Check(strings.NewReader(huge + "junk\n- 2026-11-02 18:30Z ok\n"))
	want := []Problem{{1, ReasonTooLong}, {2, ReasonPrefix}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestCheck_ReportsLineNumbersAndReasons(t *testing.T) {
	in := strings.Join([]string{
		"- 2026-11-02 18:30Z good",          // 1
		"",                                  // 2
		"# heading",                         // 3
		"- 2026-11-02 ev-785: x",            // 4
		"- 2026-11-02 ~22:45Z x",            // 5
		"2026-11-02 18:30Z x",               // 6
		"  - 2026-11-02 18:30Z x",           // 7
		"- 2026-11-02 18:30Z ",              // 8
		"- 2026-11-02 18:30Z crlf\r",        // 9
		"<<<<<<< HEAD",                      // 10
		"- 2026-11-02 18:30Z tab\tok",       // 11
		"- 2026-11-02 18:30Z bad\x1bescape", // 12
		"- 2026-11-02 18:30Z " + strings.Repeat("y", 4100), // 13
		"- 2026-11-02 18:30Z last, no newline",             // 14
	}, "\n")
	want := []Problem{
		{4, ReasonPrefix},
		{5, ReasonPrefix},
		{6, ReasonPrefix},
		{7, ReasonPrefix},
		{8, ReasonEmptyText},
		{9, ReasonControl},
		{10, ReasonConflict},
		{12, ReasonControl},
		{13, ReasonTooLong},
		{14, ReasonNoNewline},
	}
	got := Check(strings.NewReader(in))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Check =\n%v\nwant\n%v", got, want)
	}
}

func TestCheck_Clean(t *testing.T) {
	if p := Check(strings.NewReader("# t\n\n- 2026-11-02 18:30Z a\n- 2026-11-02 18:31Z b\n")); len(p) != 0 {
		t.Fatalf("problems on a clean log: %v", p)
	}
	if p := Check(strings.NewReader("")); len(p) != 0 {
		t.Fatalf("problems on an empty file: %v", p)
	}
}

func TestCheck_StampValidity(t *testing.T) {
	bad := []string{
		"- 2026-13-02 18:30Z x",
		"- 2026-11-02 18:60Z x",
		"- 2026-11-02 24:00Z x",
		"- 2026-02-30 18:30Z x",
		"- 2027-02-29 18:30Z x",
	}
	for _, l := range bad {
		got := Check(strings.NewReader(l + "\n"))
		if len(got) != 1 || got[0].Line != 1 || got[0].Reason != ReasonStamp {
			t.Errorf("%q: %v, want one ReasonStamp problem on line 1", l, got)
		}
	}
	if p := Check(strings.NewReader("- 2028-02-29 00:00Z leap\n")); len(p) != 0 {
		t.Errorf("valid leap day flagged: %v", p)
	}
}
