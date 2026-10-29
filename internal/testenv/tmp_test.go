package testenv

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

func TestHostTempDirIsFixedAtStartup(t *testing.T) {
	want := hostTmp
	t.Setenv("TMPDIR", t.TempDir())
	if got := HostTempDir(); got != want {
		t.Errorf("HostTempDir = %q after TMPDIR changed, want %q", got, want)
	}
}

func TestSmallTempDirLeavesASmallTempDirAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SmallTempDir swaps TMPDIR under /tmp; Windows reads TMP/TEMP and has no /tmp")
	}
	small := t.TempDir()
	t.Setenv("TMPDIR", small)
	cleanup := SmallTempDir()
	defer cleanup()
	if got := os.Getenv("TMPDIR"); got != small {
		t.Errorf("TMPDIR = %q, want it unchanged (%q)", got, small)
	}
}

func TestSmallTempDirSwapsABusyTempDirAndCleansUp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SmallTempDir swaps TMPDIR under /tmp; Windows reads TMP/TEMP and has no /tmp")
	}
	busy := t.TempDir()
	for i := 0; i < 205; i++ {
		if err := os.WriteFile(filepath.Join(busy, "f"+strconv.Itoa(i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TMPDIR", busy)
	cleanup := SmallTempDir()
	got := os.Getenv("TMPDIR")
	if got == busy || filepath.Dir(got) != "/tmp" {
		t.Fatalf("TMPDIR = %q, want a fresh dir under /tmp", got)
	}
	if fi, err := os.Stat(got); err != nil || !fi.IsDir() {
		t.Fatalf("swapped dir missing: %v", err)
	}
	cleanup()
	if _, err := os.Stat(got); !os.IsNotExist(err) {
		t.Errorf("cleanup left %s behind (%v)", got, err)
	}
}
