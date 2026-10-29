package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHookExpiryLogsOneLineAMinuteWithACount(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	read := func() []string {
		b, err := os.ReadFile(filepath.Join(dir, "hook-expiry.log"))
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	}
	for i := 0; i < 5; i++ {
		if err := noteHookExpiry(dir, t0.Add(time.Duration(i)*10*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if l := read(); len(l) != 1 || !strings.Contains(l[0], "count=5") {
		t.Fatalf("5 expiries inside a minute: %q, want 1 line with count=5", l)
	}
	if err := noteHookExpiry(dir, t0.Add(61*time.Second)); err != nil {
		t.Fatal(err)
	}
	if l := read(); len(l) != 2 || !strings.Contains(l[0], "count=5") || !strings.Contains(l[1], "count=1") {
		t.Fatalf("after the minute: %q, want a second line with count=1", l)
	}
}

// A hook that runs out of time leaves the trace in the first repo's state dir.
func TestHook_DeadlineLeavesATrace(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	oldHook, oldDone := hookTestHook, hookWorkDone
	worker := make(chan struct{})
	hookTestHook = func() { time.Sleep(time.Second) }
	hookWorkDone = func() { close(worker) }
	defer func() { <-worker; hookTestHook, hookWorkDone = oldHook, oldDone }()
	in := hookJSON("s", "/home/alice/src/app", "UserPromptSubmit")
	if code, out, _ := runHook(t, in, "--config", cfgPath, "--hook", "--deadline", "100ms"); code != 0 || out != "" {
		t.Fatalf("exit %d stdout %q", code, out)
	}
	cfg, _ := loadDistillConfig(cfgPath)
	b, err := os.ReadFile(filepath.Join(cfg.SyncDir, ".state", "hook-expiry.log"))
	if err != nil || !strings.Contains(string(b), "count=1") {
		t.Fatalf("no trace: %q %v", b, err)
	}
}
