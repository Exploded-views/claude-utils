//go:build !windows

package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCfgMode(t *testing.T, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(`{"scrub":false}`), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad_RefusesAConfigOthersCanReachWithoutGit(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o640, 0o602, 0o666, 0o604} {
		p := writeCfgMode(t, mode)
		_, err := Load(p)
		if err == nil {
			t.Errorf("mode %04o: Load must refuse", mode)
			continue
		}
		for _, want := range []string{p, "0600", "chmod 600"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("mode %04o: error %q should mention %q", mode, err, want)
			}
		}
	}
}

func TestLoad_AcceptsAPrivateOwnConfig(t *testing.T) {
	for _, mode := range []os.FileMode{0o600, 0o400} {
		if _, err := Load(writeCfgMode(t, mode)); err != nil {
			t.Errorf("mode %04o: %v", mode, err)
		}
	}
}

func TestLoad_RefusesAConfigOwnedBySomeoneElse(t *testing.T) {
	p := writeCfgMode(t, 0o600)
	old := currentUID
	currentUID = func() int { return os.Getuid() + 1 }
	t.Cleanup(func() { currentUID = old })
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "owned by") || !strings.Contains(err.Error(), p) {
		t.Errorf("want an ownership error naming the file, got %v", err)
	}
}

func TestLoad_FollowsASymlinkToTheRealFile(t *testing.T) {
	p := writeCfgMode(t, 0o644)
	l := filepath.Join(t.TempDir(), "link.json")
	if err := os.Symlink(p, l); err != nil {
		t.Skip(err)
	}
	if _, err := Load(l); err == nil {
		t.Error("a link to a 0644 file must be refused")
	}
}

func TestLoad_AcceptsALinkToAPrivateFile(t *testing.T) {
	p := writeCfgMode(t, 0o600)
	l := filepath.Join(t.TempDir(), "link.json")
	if err := os.Symlink(p, l); err != nil {
		t.Skip(err)
	}
	if _, err := Load(l); err != nil {
		t.Errorf("the target is what counts: %v", err)
	}
}

// A path that is still a literal "~/..." is an error even when a directory
// called "~" exists where the process runs.
func TestLoad_RefusesAnUnexpandedTilde(t *testing.T) {
	d := t.TempDir()
	if err := os.MkdirAll(filepath.Join(d, "~", "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(d, "~", "x", "config.json"), []byte(`{}`), 0o600)
	t.Chdir(d)
	if _, err := Load("~/x/config.json"); err == nil || !strings.Contains(err.Error(), "not expanded") {
		t.Errorf("want a refusal of the literal ~ path, got %v", err)
	}
}

// fakeInfo is a FileInfo whose Sys() is not a *syscall.Stat_t, as on a
// filesystem that cannot say who owns a file.
type fakeInfo struct{ os.FileInfo }

func (fakeInfo) Sys() any { return nil }

func TestCheckPrivate_RefusesWhenTheOwnerCannotBeDetermined(t *testing.T) {
	p := writeCfgMode(t, 0o600)
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	err = checkPrivate(p, fakeInfo{st})
	if !errors.Is(err, ErrNotPrivate) || !strings.Contains(err.Error(), "cannot tell who owns") {
		t.Errorf("want ErrNotPrivate about unknown ownership, got %v", err)
	}
}

func TestCheckPrivate_ModeMessageAndOwnerOnlyPass(t *testing.T) {
	p := writeCfgMode(t, 0o640)
	st, _ := os.Stat(p)
	err := checkPrivate(p, st)
	if !errors.Is(err, ErrNotPrivate) || !strings.Contains(err.Error(), "mode 0640") {
		t.Errorf("want ErrNotPrivate naming mode 0640, got %v", err)
	}
	p = writeCfgMode(t, 0o600)
	st, _ = os.Stat(p)
	if err := checkPrivate(p, st); err != nil {
		t.Errorf("an owner-only file of ours must pass: %v", err)
	}
}

func TestCheckPrivate_OwnershipErrorNamesBothUIDs(t *testing.T) {
	p := writeCfgMode(t, 0o600)
	st, _ := os.Stat(p)
	old := currentUID
	currentUID = func() int { return os.Getuid() + 1 }
	t.Cleanup(func() { currentUID = old })
	err := checkPrivate(p, st)
	want := fmt.Sprintf("owned by uid %d", os.Getuid())
	if !errors.Is(err, ErrNotPrivate) || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), fmt.Sprintf("uid %d)", os.Getuid()+1)) {
		t.Errorf("want ErrNotPrivate with %q and the current uid, got %v", want, err)
	}
}

func TestLoad_ErrNotPrivateIsMatchable(t *testing.T) {
	if _, err := Load(writeCfgMode(t, 0o644)); !errors.Is(err, ErrNotPrivate) {
		t.Errorf("want errors.Is ErrNotPrivate, got %v", err)
	}
}

func TestSaveAt_FailsInAReadOnlyDirectoryAndLeavesNoFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so this would pass vacuously")
	}
	d := t.TempDir()
	if err := os.Chmod(d, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(d, 0o700) })
	if err := SaveAt(filepath.Join(d, "config.json"), Defaults()); err == nil {
		t.Fatal("writing into a read-only directory must fail")
	}
	if _, err := os.Stat(filepath.Join(d, "config.json")); err == nil {
		t.Error("a failed save must not leave a config.json")
	}
}

func TestLoad_FailsOnAnUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read any file, so this would pass vacuously")
	}
	p := writeCfgMode(t, 0o000)
	if _, err := Load(p); !errors.Is(err, os.ErrPermission) {
		t.Errorf("want a permission error, got %v", err)
	}
}
