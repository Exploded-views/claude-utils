package config

import (
	"os"
	"path/filepath"
	"runtime"
)

// AppDirName is the per-host config directory's name.
const AppDirName = "claude-memsync"

// Test seams: the OS name and os.UserConfigDir.
var (
	GOOS          = runtime.GOOS
	UserConfigDir = os.UserConfigDir
)

// Dir is the per-host config directory, never synced: it holds routes.json,
// apps.json, the age identity and keys. It is $XDG_CONFIG_HOME/claude-memsync,
// else ~/.config/claude-memsync, on every OS (Windows included, so the
// directory is the same wherever the binary runs). It is not created.
func Dir() (string, error) {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, AppDirName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", AppDirName), nil
}

// LegacyDir is where earlier builds kept the age identity: os.UserConfigDir
// ("~/Library/Application Support" on macOS, %AppData% on Windows). It is ""
// on other systems, where that was ~/.config already.
func LegacyDir() string {
	switch GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		return filepath.Join(home, "Library", "Application Support", AppDirName)
	case "windows":
		d, err := UserConfigDir()
		if err != nil {
			return ""
		}
		return filepath.Join(d, AppDirName)
	}
	return ""
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// Resolve is the path of the config file name. If neither name nor any of alts
// is in Dir() but one is in LegacyDir(), the legacy path is returned with
// legacy set, so an existing install keeps its file instead of getting a second
// one. Nothing is created or moved. With no Dir (no home), the result is name
// alone, relative.
func Resolve(name string, alts ...string) (path string, legacy bool) {
	dir, err := Dir()
	if err != nil {
		return name, false
	}
	names := append([]string{name}, alts...)
	for _, n := range names {
		if isFile(filepath.Join(dir, n)) {
			return filepath.Join(dir, name), false
		}
	}
	if ld := LegacyDir(); ld != "" {
		for _, n := range names {
			if isFile(filepath.Join(ld, n)) {
				return filepath.Join(ld, name), true
			}
		}
	}
	return filepath.Join(dir, name), false
}

