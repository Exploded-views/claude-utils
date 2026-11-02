package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kardianos/service"

	"github.com/MarimerLLC/claude-utils/internal/stoprec"
)

func statusNotInstalled(t *testing.T) {
	t.Helper()
	old := statusFn
	statusFn = func() (string, error) { return "", service.ErrNotInstalled }
	t.Cleanup(func() { statusFn = old })
}

func TestStatus_ShowsLastUncleanStop(t *testing.T) {
	dir := stopHome(t)
	statusNotInstalled(t)
	when := time.Date(2026, 11, 2, 12, 30, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if err := stoprec.Write(dir, stoprec.Record{Result: "exit-code", ExitCode: "exited", ExitStatus: "1"}, when.Add(time.Duration(i)*time.Minute), stoprec.Default); err != nil {
			t.Fatal(err)
		}
	}
	code, out := captureStdout(t, func() int { return runStatus(nil) })
	want := "last unclean stop: 2026-11-02T12:32:00Z exit-code (exit 1, ×3); records in " + dir
	if !strings.Contains(out, want) {
		t.Fatalf("status has no %q line:\n%s", want, out)
	}
	if code != 0 {
		t.Fatalf("a record changed the exit code to %d", code)
	}
}

func TestStatus_SingleStopHasNoRepeatCount(t *testing.T) {
	dir := stopHome(t)
	statusNotInstalled(t)
	stoprec.Write(dir, stoprec.Record{Result: "timeout"}, time.Date(2026, 11, 2, 1, 2, 3, 0, time.UTC), stoprec.Default)
	_, out := captureStdout(t, func() int { return runStatus(nil) })
	if !strings.Contains(out, "last unclean stop: 2026-11-02T01:02:03Z timeout; records in "+dir) {
		t.Fatalf("out:\n%s", out)
	}
}

func TestStatus_NoRecordNoLine(t *testing.T) {
	stopHome(t)
	statusNotInstalled(t)
	code, out := captureStdout(t, func() int { return runStatus(nil) })
	if strings.Contains(out, "unclean stop") || code != 0 {
		t.Fatalf("code=%d out:\n%s", code, out)
	}
}

func TestStatus_UnreadableRecordsDirIsSilent(t *testing.T) {
	dir := stopHome(t)
	statusNotInstalled(t)
	os.MkdirAll(filepath.Dir(dir), 0o700)
	os.WriteFile(dir, []byte("x"), 0o600) // a file where the dir belongs
	code, out := captureStdout(t, func() int { return runStatus(nil) })
	if strings.Contains(out, "unclean stop") || code != 0 {
		t.Fatalf("code=%d out:\n%s", code, out)
	}
}
