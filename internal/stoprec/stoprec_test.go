package stoprec

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/scrub"
)

var t0 = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

func newDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "stops")
}

func mustWrite(t *testing.T, dir string, rec Record, now time.Time) {
	t.Helper()
	if err := Write(dir, rec, now, Default); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

func readAll(t *testing.T, dir string) (names []string, all []byte) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		names = append(names, e.Name())
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, b...)
	}
	return names, all
}

func TestDefaultLimits(t *testing.T) {
	want := Limits{MaxLines: 80, MaxLineBytes: 512, MaxRecordBytes: 16 << 10, MaxRecords: 20, Collapse: time.Hour}
	if Default != want {
		t.Fatalf("Default = %+v, want %+v", Default, want)
	}
}

func TestDir_IsUnderTheConfigDir(t *testing.T) {
	x := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", x)
	got, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(x, "claude-memsync", "stops"); got != want {
		t.Fatalf("Dir() = %s, want %s", got, want)
	}
}

func TestEnsureDir_CreatesPrivateDirAndIsIdempotent(t *testing.T) {
	d := newDir(t)
	if err := EnsureDir(d); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDir(d); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(d)
		if err != nil || st.Mode().Perm() != 0o700 {
			t.Fatalf("dir mode = %v, %v; want 0700", st.Mode().Perm(), err)
		}
	}
}

func TestEnsureDir_RefusesAFile(t *testing.T) {
	d := newDir(t)
	os.WriteFile(d, []byte("x"), 0o600)
	if err := EnsureDir(d); err == nil {
		t.Fatal("a file in place of the dir was accepted")
	}
}

func TestWrite_FileIs0600AndLeavesNoTemp(t *testing.T) {
	d := newDir(t)
	mustWrite(t, d, Record{Result: "exit-code", ExitStatus: "1"}, t0)
	names, _ := readAll(t, d)
	if len(names) != 1 || !strings.HasSuffix(names[0], ".json") || strings.HasPrefix(names[0], ".") {
		t.Fatalf("files = %v, want one record and no temp file", names)
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(filepath.Join(d, names[0]))
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("record mode = %v, want 0600", st.Mode().Perm())
		}
	}
}

func TestWrite_RenameFailureLeavesNothing(t *testing.T) {
	d := newDir(t)
	old := renameFn
	renameFn = func(a, b string) error { return errors.New("injected") }
	defer func() { renameFn = old }()
	if err := Write(d, Record{Result: "exit-code"}, t0, Default); err == nil {
		t.Fatal("Write hid the rename failure")
	}
	names, _ := readAll(t, d)
	if len(names) != 0 {
		t.Fatalf("a failed write left %v", names)
	}
}

func TestWrite_SizeCapPerRecord(t *testing.T) {
	d := newDir(t)
	var tail []string
	for i := 0; i < 20000; i++ {
		tail = append(tail, fmt.Sprintf("line-%05d %s", i, strings.Repeat("x", 480)))
	}
	tail = append(tail, "huge "+strings.Repeat("y", 5000))
	tail = append(tail, "the-newest-line")
	mustWrite(t, d, Record{Result: "exit-code", Tail: tail}, t0)
	names, all := readAll(t, d)
	if len(all) > 16<<10 {
		t.Fatalf("record is %d bytes, cap 16384", len(all))
	}
	rec, ok, err := Latest(d)
	if err != nil || !ok {
		t.Fatalf("Latest: %v %v (%v)", rec, ok, err)
	}
	if len(rec.Tail) == 0 || len(rec.Tail) > 80 {
		t.Fatalf("tail has %d lines, want 1..80", len(rec.Tail))
	}
	for _, l := range rec.Tail {
		if len(l) > 512 {
			t.Fatalf("kept line has %d bytes, cap 512", len(l))
		}
	}
	if last := rec.Tail[len(rec.Tail)-1]; last != "the-newest-line" {
		t.Fatalf("newest line not kept: %q", last)
	}
	_ = names
}

func TestWrite_LineCap80(t *testing.T) {
	d := newDir(t)
	var tail []string
	for i := 0; i < 200; i++ {
		tail = append(tail, fmt.Sprintf("l%03d", i))
	}
	mustWrite(t, d, Record{Result: "exit-code", Tail: tail}, t0)
	rec, _, _ := Latest(d)
	if len(rec.Tail) != 80 || rec.Tail[0] != "l120" || rec.Tail[79] != "l199" {
		t.Fatalf("tail = %d lines, first %q last %q; want the newest 80", len(rec.Tail), rec.Tail[0], rec.Tail[len(rec.Tail)-1])
	}
}

func TestWrite_DirCapEvictsOldest(t *testing.T) {
	d := newDir(t)
	for i := 1; i <= 50; i++ {
		mustWrite(t, d, Record{Result: "exit-code", ExitStatus: fmt.Sprint(i)}, t0.Add(time.Duration(i)*2*time.Hour))
	}
	names, all := readAll(t, d)
	if len(names) != 20 || len(all) > 20*(16<<10) {
		t.Fatalf("%d files, %d bytes; want 20 files", len(names), len(all))
	}
	recs, err := List(d)
	if err != nil || len(recs) != 20 {
		t.Fatalf("List = %d, %v", len(recs), err)
	}
	if recs[0].ExitStatus != "50" || recs[19].ExitStatus != "31" {
		t.Fatalf("kept %s..%s, want the newest 20 (50..31), newest first", recs[0].ExitStatus, recs[19].ExitStatus)
	}
}

func TestWrite_CollapsesRepeats(t *testing.T) {
	d := newDir(t)
	for i := 0; i < 100; i++ {
		mustWrite(t, d, Record{Result: "exit-code", ExitCode: "exited", ExitStatus: "1", Tail: []string{fmt.Sprint("run ", i)}}, t0.Add(time.Duration(i)*30*time.Second))
	}
	names, _ := readAll(t, d)
	if len(names) != 1 {
		t.Fatalf("%d files, want 1", len(names))
	}
	rec, _, _ := Latest(d)
	if rec.Repeats != 100 || !rec.When.Equal(t0) || !rec.Last.Equal(t0.Add(99*30*time.Second)) {
		t.Fatalf("repeats=%d when=%v last=%v", rec.Repeats, rec.When, rec.Last)
	}
	if len(rec.Tail) != 1 || rec.Tail[0] != "run 99" {
		t.Fatalf("tail = %v, want the newest", rec.Tail)
	}
}

func TestWrite_NoCollapseAfterAnHourOrWhenDifferent(t *testing.T) {
	d := newDir(t)
	mustWrite(t, d, Record{Result: "exit-code", ExitStatus: "1"}, t0)
	mustWrite(t, d, Record{Result: "exit-code", ExitStatus: "1"}, t0.Add(61*time.Minute))
	mustWrite(t, d, Record{Result: "exit-code", ExitStatus: "2"}, t0.Add(62*time.Minute))
	mustWrite(t, d, Record{Result: "signal", ExitStatus: "1"}, t0.Add(63*time.Minute))
	if names, _ := readAll(t, d); len(names) != 4 {
		t.Fatalf("%d files, want 4", len(names))
	}
}

func TestWrite_RefusesAnInsecureDirAndWritesNothing(t *testing.T) {
	d := newDir(t)
	os.WriteFile(d, []byte("x"), 0o600)
	if err := Write(d, Record{Result: "exit-code"}, t0, Default); err == nil {
		t.Fatal("Write into a non-directory succeeded")
	}
}

func TestLatest_EmptyAndMissing(t *testing.T) {
	if _, ok, err := Latest(filepath.Join(t.TempDir(), "none")); ok || err != nil {
		t.Fatalf("missing dir: ok=%v err=%v", ok, err)
	}
	d := newDir(t)
	EnsureDir(d)
	if _, ok, err := Latest(d); ok || err != nil {
		t.Fatalf("empty dir: ok=%v err=%v", ok, err)
	}
}

func TestLatest_IgnoresForeignAndCorruptFiles(t *testing.T) {
	d := newDir(t)
	mustWrite(t, d, Record{Result: "timeout"}, t0)
	os.WriteFile(filepath.Join(d, "notes.txt"), []byte("hi"), 0o600)
	os.WriteFile(filepath.Join(d, "stop-29990101T000000Z.json"), []byte("{broken"), 0o600)
	rec, ok, err := Latest(d)
	if err != nil || !ok || rec.Result != "timeout" {
		t.Fatalf("Latest = %+v %v %v", rec, ok, err)
	}
}

func TestRecordCarriesThePrivacyNote(t *testing.T) {
	d := newDir(t)
	mustWrite(t, d, Record{Result: "timeout"}, t0)
	_, all := readAll(t, d)
	if !bytes.Contains(all, []byte("review")) {
		t.Fatalf("record header has no review note:\n%s", all)
	}
}

// Secret fixtures are built at run time so this file holds no token.
func secretLines() (ghp, url, pem string) {
	return "token is ghp_" + strings.Repeat("a", 36) + " ok",
		"clone https://user:" + "hunter2pass" + "@example.com/x failed",
		"-----BEGIN " + "RSA PRIVATE KEY-----"
}

func TestScrubTail_WithholdsSecretLines(t *testing.T) {
	ghp, url, pem := secretLines()
	in := []string{"plain line", ghp, url, pem, "another plain"}
	kept, withheld := ScrubTail(in, scrub.New(nil))
	if withheld != 3 || len(kept) != 5 {
		t.Fatalf("withheld=%d kept=%d, want 3 and 5", withheld, len(kept))
	}
	want := []string{"plain line", "[line withheld: github-token]", "[line withheld: url-credentials]", "[line withheld: private-key]", "another plain"}
	for i := range want {
		if kept[i] != want[i] {
			t.Fatalf("kept[%d] = %q, want %q", i, kept[i], want[i])
		}
	}
}

func TestScrubTail_RedactsDenylistedPaths(t *testing.T) {
	deny, err := scrub.ParseDenylist(strings.NewReader("acme-secret-plans\n"))
	if err != nil {
		t.Fatal(err)
	}
	kept, withheld := ScrubTail([]string{"git: bad ref in projects/acme-secret-plans/notes.md today", "fine"}, scrub.New(deny))
	if withheld != 0 {
		t.Fatalf("withheld = %d", withheld)
	}
	if strings.Contains(kept[0], "acme-secret-plans") || !strings.Contains(kept[0], "<path redacted>") || !strings.Contains(kept[0], "today") {
		t.Fatalf("line = %q", kept[0])
	}
	if kept[1] != "fine" {
		t.Fatalf("kept[1] = %q", kept[1])
	}
}

func TestScrubTail_UnreadableDenylistWithholdsPathLikeTokens(t *testing.T) {
	kept, _ := ScrubTail([]string{"error in /home/u/notes/private.md at start", "no path here"}, nil)
	if strings.Contains(kept[0], "private.md") || !strings.Contains(kept[0], "<path redacted>") || !strings.Contains(kept[0], "at start") {
		t.Fatalf("line = %q", kept[0])
	}
	if kept[1] != "no path here" {
		t.Fatalf("kept[1] = %q", kept[1])
	}
	// The built-in rules still apply with no scanner.
	ghp, _, _ := secretLines()
	k, w := ScrubTail([]string{ghp}, nil)
	if w != 1 || strings.Contains(k[0], "ghp_") {
		t.Fatalf("nil scanner let a token through: %q", k[0])
	}
}

func TestScrubTail_NoSecretByteReachesTheFile(t *testing.T) {
	ghp, url, pem := secretLines()
	deny, _ := scrub.ParseDenylist(strings.NewReader("acme-secret-plans\n"))
	kept, withheld := ScrubTail([]string{ghp, url, pem, "see acme-secret-plans/x"}, scrub.New(deny))
	d := newDir(t)
	mustWrite(t, d, Record{Result: "exit-code", Tail: kept, Withheld: withheld}, t0)
	_, all := readAll(t, d)
	for _, bad := range []string{"ghp_", "hunter2pass", "PRIVATE KEY", "acme-secret-plans"} {
		if bytes.Contains(all, []byte(bad)) {
			t.Fatalf("the record holds %q", bad)
		}
	}
	rec, _, _ := Latest(d)
	if rec.Withheld != 3 {
		t.Fatalf("Withheld = %d, want 3", rec.Withheld)
	}
}
