package config

import (
	"os"
	"path/filepath"
	"testing"
)

func setPlatform(t *testing.T, goos, userCfg string) {
	t.Helper()
	og, ou := GOOS, UserConfigDir
	GOOS = goos
	UserConfigDir = func() (string, error) { return userCfg, nil }
	t.Cleanup(func() { GOOS, UserConfigDir = og, ou })
}

func write(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDir_XDGSet(t *testing.T) {
	x := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", x)
	t.Setenv("HOME", t.TempDir())
	got, err := Dir()
	if err != nil || got != filepath.Join(x, "claude-memsync") {
		t.Fatalf("Dir() = %q, %v", got, err)
	}
}

func TestDir_XDGUnsetUsesDotConfigOnEveryOS(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, goos := range []string{"linux", "darwin", "windows"} {
		setPlatform(t, goos, filepath.Join(home, "AppData-or-Library"))
		got, err := Dir()
		if want := filepath.Join(home, ".config", "claude-memsync"); err != nil || got != want {
			t.Fatalf("%s: Dir() = %q, %v; want %q", goos, got, err, want)
		}
	}
}

func TestResolve_LegacyFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	newP := filepath.Join(home, ".config", "claude-memsync", "identity.txt")
	macLegacy := filepath.Join(home, "Library", "Application Support", "claude-memsync", "identity.txt")
	winCfg := filepath.Join(home, "AppData", "Roaming")
	winLegacy := filepath.Join(winCfg, "claude-memsync", "identity.txt")

	t.Run("darwin legacy only", func(t *testing.T) {
		setPlatform(t, "darwin", filepath.Join(home, "Library", "Application Support"))
		write(t, macLegacy)
		defer os.RemoveAll(filepath.Join(home, "Library"))
		got, legacy := Resolve("identity.txt")
		if got != macLegacy || !legacy {
			t.Fatalf("Resolve = %q, %v; want legacy %q", got, legacy, macLegacy)
		}
	})
	t.Run("darwin legacy holds only the pq file", func(t *testing.T) {
		setPlatform(t, "darwin", filepath.Join(home, "Library", "Application Support"))
		write(t, filepath.Join(filepath.Dir(macLegacy), "identity-pq.txt"))
		defer os.RemoveAll(filepath.Join(home, "Library"))
		got, legacy := Resolve("identity.txt", "identity-pq.txt")
		if got != macLegacy || !legacy {
			t.Fatalf("Resolve = %q, %v; want legacy %q", got, legacy, macLegacy)
		}
	})
	t.Run("darwin new path wins when both exist", func(t *testing.T) {
		setPlatform(t, "darwin", filepath.Join(home, "Library", "Application Support"))
		write(t, macLegacy)
		write(t, newP)
		defer os.RemoveAll(filepath.Join(home, "Library"))
		defer os.RemoveAll(filepath.Join(home, ".config"))
		got, legacy := Resolve("identity.txt")
		if got != newP || legacy {
			t.Fatalf("Resolve = %q, %v; want %q", got, legacy, newP)
		}
	})
	t.Run("darwin neither exists: new path, nothing created", func(t *testing.T) {
		setPlatform(t, "darwin", filepath.Join(home, "Library", "Application Support"))
		got, legacy := Resolve("identity.txt")
		if got != newP || legacy {
			t.Fatalf("Resolve = %q, %v; want %q", got, legacy, newP)
		}
		if _, err := os.Stat(filepath.Dir(newP)); err == nil {
			t.Fatal("Resolve created the config dir")
		}
	})
	t.Run("linux ignores a Library dir", func(t *testing.T) {
		setPlatform(t, "linux", filepath.Join(home, ".config"))
		write(t, macLegacy)
		defer os.RemoveAll(filepath.Join(home, "Library"))
		got, legacy := Resolve("identity.txt")
		if got != newP || legacy {
			t.Fatalf("Resolve = %q, %v; want %q", got, legacy, newP)
		}
	})
	t.Run("windows legacy is the AppData dir", func(t *testing.T) {
		setPlatform(t, "windows", winCfg)
		write(t, winLegacy)
		got, legacy := Resolve("identity.txt")
		if got != winLegacy || !legacy {
			t.Fatalf("Resolve = %q, %v; want legacy %q", got, legacy, winLegacy)
		}
	})
}
