//go:build !windows

package stoprec

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestEnsureDir_PrivateModes(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)

	d := newDir(t)
	if err := EnsureDir(d); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(d); st.Mode().Perm() != 0o700 {
		t.Fatalf("new dir mode = %v under umask 0, want 0700", st.Mode().Perm())
	}

	// A wider dir we own is tightened.
	os.Chmod(d, 0o755)
	if err := EnsureDir(d); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(d); st.Mode().Perm() != 0o700 {
		t.Fatalf("mode = %v after EnsureDir, want 0700", st.Mode().Perm())
	}

	// A record is 0600 even under umask 0.
	mustWrite(t, d, Record{Result: "exit-code"}, t0)
	names, _ := readAll(t, d)
	if st, _ := os.Stat(filepath.Join(d, names[0])); st.Mode().Perm() != 0o600 {
		t.Fatalf("record mode = %v under umask 0", st.Mode().Perm())
	}
}

func TestEnsureDir_RefusesASymlink(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "stops")
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	if err := EnsureDir(link); err == nil {
		t.Fatal("a symlinked dir was accepted")
	}
	if err := Write(link, Record{Result: "exit-code"}, t0, Default); err == nil {
		t.Fatal("Write followed a symlinked dir")
	}
	if ents, _ := os.ReadDir(real); len(ents) != 0 {
		t.Fatalf("wrote through the link: %v", ents)
	}
}

func TestEnsureDir_RefusesAnotherOwner(t *testing.T) {
	d := newDir(t)
	if err := EnsureDir(d); err != nil {
		t.Fatal(err)
	}
	old := currentUID
	currentUID = func() int { return os.Getuid() + 1 }
	defer func() { currentUID = old }()
	if err := EnsureDir(d); err == nil {
		t.Fatal("a dir owned by another uid was accepted")
	}
	if err := Write(d, Record{Result: "exit-code"}, t0, Default); err == nil {
		t.Fatal("Write into another user's dir succeeded")
	}
}

func TestWrite_PlantedSymlinkAtTheRecordNameIsNotFollowed(t *testing.T) {
	d := newDir(t)
	if err := EnsureDir(d); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "victim")
	os.WriteFile(target, []byte("keep"), 0o600)
	// Pre-plant a symlink at the file name the writer will pick.
	os.Symlink(target, filepath.Join(d, recordName(t0, 0)))
	if err := Write(d, Record{Result: "exit-code"}, t0, Default); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Fatalf("the writer followed a symlink: %q", b)
	}
}

func TestLatest_RefusesASymlinkedDirAndAForeignOwner(t *testing.T) {
	real := filepath.Join(t.TempDir(), "real")
	mustWrite(t, real, Record{Result: "exit-code"}, t0)
	link := filepath.Join(t.TempDir(), "stops")
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	if _, ok, err := Latest(link); ok || err == nil {
		t.Fatalf("read through a symlinked dir: ok=%v err=%v", ok, err)
	}
	old := currentUID
	currentUID = func() int { return os.Getuid() + 1 }
	defer func() { currentUID = old }()
	if _, ok, err := Latest(real); ok || err == nil {
		t.Fatalf("read another user's dir: ok=%v err=%v", ok, err)
	}
}
