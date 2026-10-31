//go:build !windows

package config

import (
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
