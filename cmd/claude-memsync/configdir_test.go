package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/ghapp"
	"github.com/kardianos/service"
)

func cfgPlatform(t *testing.T, goos, userCfg string) {
	t.Helper()
	og, ou := config.GOOS, config.UserConfigDir
	config.GOOS = goos
	config.UserConfigDir = func() (string, error) { return userCfg, nil }
	t.Cleanup(func() { config.GOOS, config.UserConfigDir = og, ou })
}

// Routes, apps and the identity share one directory, with XDG set and unset,
// whatever the OS's own idea of a user config dir is.
func TestConfigPaths_ShareOneDir(t *testing.T) {
	for _, xdg := range []string{"", filepath.Join(t.TempDir(), "xdg")} {
		for _, goos := range []string{"linux", "darwin", "windows"} {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("XDG_CONFIG_HOME", xdg)
			cfgPlatform(t, goos, filepath.Join(home, "OSConfig"))
			want, err := config.Dir()
			if err != nil {
				t.Fatal(err)
			}
			apps, err := ghapp.DefaultAppsPath()
			if err != nil {
				t.Fatal(err)
			}
			for name, p := range map[string]string{
				"routes": defaultRoutesPath(), "apps": apps, "identity": defaultIdentityPath(),
			} {
				if filepath.Dir(p) != want {
					t.Errorf("xdg=%q %s: %s is in %s, want %s", xdg, goos, name, filepath.Dir(p), want)
				}
			}
		}
	}
}

func TestDefaultIdentityPath_DarwinLegacyIsReadNotDuplicated(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	legacy := filepath.Join(home, "Library", "Application Support", "claude-memsync", "identity.txt")
	os.MkdirAll(filepath.Dir(legacy), 0o700)
	os.WriteFile(legacy, []byte("x"), 0o600)
	cfgPlatform(t, "darwin", filepath.Join(home, "Library", "Application Support"))
	if got := defaultIdentityPath(); got != legacy {
		t.Fatalf("defaultIdentityPath = %s, want the existing legacy file %s", got, legacy)
	}
	if got := defaultIdentityFor("pq"); got != legacy {
		t.Fatalf("defaultIdentityFor(pq) = %s, want %s", got, legacy)
	}
	// Nothing was moved or copied.
	if _, err := os.Stat(filepath.Join(home, ".config")); err == nil {
		t.Fatal("a config dir was created")
	}
}

func TestStatus_LegacyIdentityHintOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv(routesEnv, "")
	legacy := filepath.Join(home, "Library", "Application Support", "claude-memsync", "identity.txt")
	os.MkdirAll(filepath.Dir(legacy), 0o700)
	os.WriteFile(legacy, []byte("x"), 0o600)
	cfgPlatform(t, "darwin", filepath.Join(home, "Library", "Application Support"))
	old := statusFn
	statusFn = func() (string, error) { return "", service.ErrNotInstalled }
	t.Cleanup(func() { statusFn = old })

	_, out := captureStdout(t, func() int { return runStatus(nil) })
	if !strings.Contains(out, "hint:") || !strings.Contains(out, legacy) {
		t.Fatalf("first status has no legacy-identity hint:\n%s", out)
	}
	_, out = captureStdout(t, func() int { return runStatus(nil) })
	if strings.Contains(out, "hint:") {
		t.Fatalf("hint repeated:\n%s", out)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatal("status moved the identity")
	}
}
