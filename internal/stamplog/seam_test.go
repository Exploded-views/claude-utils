package stamplog

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/lock"
)

func TestAppend_FileReplacedEveryTimeGivesUp(t *testing.T) {
	old := sameFileHook
	defer func() { sameFileHook = old }()
	sameFileHook = func(a, b os.FileInfo) bool { return false }
	p := filepath.Join(t.TempDir(), "log.md")
	os.WriteFile(p, nil, 0600)
	err := Append(p, "x", time.Now(), Options{})
	if !errors.Is(err, ErrFile) {
		t.Fatalf("err = %v, want ErrFile", err)
	}
	if mustRead(t, p) != "" {
		t.Fatal("wrote anyway")
	}
}

type failingWriter struct{ f *os.File }

func (w failingWriter) WriteString(s string) (int, error) {
	n, _ := w.f.WriteString(s[:7]) // a torn write
	return n, errors.New("disk full")
}

func TestAppend_PartialWriteIsTruncatedAway(t *testing.T) {
	old := writeEntry
	defer func() { writeEntry = old }()
	writeEntry = func(f *os.File, s string) (int, error) { return failingWriter{f}.WriteString(s) }
	p := filepath.Join(t.TempDir(), "log.md")
	before := "- 2026-11-02 18:29Z first\n"
	os.WriteFile(p, []byte(before), 0600)
	if err := Append(p, "x", time.Now(), Options{}); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v", err)
	}
	if got := mustRead(t, p); got != before {
		t.Fatalf("fragment left behind: %q", got)
	}
	writeEntry = old
	if err := Append(p, "next", time.Now(), Options{}); err != nil {
		t.Fatalf("later append refused: %v", err)
	}
}

func TestLockErrorIsReportedAtOnce(t *testing.T) {
	old := waitFile
	defer func() { waitFile = old }()
	hard := errors.New("operation not supported")
	waitFile = func(f *os.File, d time.Duration) (func(), error) { return nil, hard }
	p := filepath.Join(t.TempDir(), "log.md")
	start := time.Now()
	err := Append(p, "x", time.Now(), Options{LockTimeout: 5 * time.Second})
	if !errors.Is(err, hard) || errors.Is(err, lock.ErrTimeout) || time.Since(start) > time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
}
