//go:build !windows

package stamplog

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestAppend_CreatesPrivateKeepsExistingMode(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	dir := t.TempDir()
	fresh := filepath.Join(dir, "new.md")
	if err := Append(fresh, "x", time.Now(), Options{}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(fresh); fi.Mode().Perm() != 0600 {
		t.Errorf("new file mode = %v, want 0600", fi.Mode().Perm())
	}
	existing := filepath.Join(dir, "old.md")
	os.WriteFile(existing, nil, 0644)
	os.Chmod(existing, 0644)
	if err := Append(existing, "x", time.Now(), Options{}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(existing); fi.Mode().Perm() != 0644 {
		t.Errorf("existing file mode = %v, want 0644", fi.Mode().Perm())
	}
}

func TestAppend_FollowsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.md")
	os.WriteFile(target, nil, 0600)
	link := filepath.Join(dir, "link.md")
	os.Symlink(target, link)
	if err := Append(link, "x", time.Now(), Options{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); len(b) == 0 {
		t.Fatal("nothing written through the symlink")
	}
}

func TestAppend_RefusesFIFO(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(p, 0600); err != nil {
		t.Skip("mkfifo:", err)
	}
	done := make(chan error, 1)
	go func() { done <- Append(p, "x", time.Now(), Options{}) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrFile) {
			t.Fatalf("err = %v, want ErrFile", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("append to a FIFO blocked")
	}
}

func TestAppend_RefusesCharacterDevice(t *testing.T) {
	if err := Append("/dev/null", "x", time.Now(), Options{}); !errors.Is(err, ErrFile) {
		t.Fatalf("err = %v, want ErrFile", err)
	}
}
