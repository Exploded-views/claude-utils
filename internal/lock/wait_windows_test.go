//go:build windows

package lock

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// WaitFile must not lock bytes a reader needs: another handle can read the
// whole file while the lock is held.
func TestWaitFile_ReadersAreNotBlocked(t *testing.T) {
	p := filepath.Join(t.TempDir(), "log")
	if err := os.WriteFile(p, []byte("- entry\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rel, err := WaitFile(f, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "- entry\n" {
		t.Fatalf("read while locked: %q, %v", b, err)
	}
}
