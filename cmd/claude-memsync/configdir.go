package main

import (
	"os"
	"path/filepath"

	"github.com/MarimerLLC/claude-utils/internal/config"
)

const legacyHintMarker = ".legacy-identity-hint-shown"

// legacyIdentityHint is a one-time line for a host whose age identity is still
// in the pre-XDG location (~/Library/Application Support on macOS, %AppData% on
// Windows). It never moves the file: the identity is in use. The marker file
// lives in the new config dir; if it cannot be written the hint just repeats.
func legacyIdentityHint() string {
	path, legacy := config.Resolve("identity.txt", "identity-pq.txt")
	if !legacy {
		return ""
	}
	dir, err := config.Dir()
	if err != nil {
		return ""
	}
	marker := filepath.Join(dir, legacyHintMarker)
	if _, err := os.Stat(marker); err == nil {
		return ""
	}
	if os.MkdirAll(dir, 0o700) == nil {
		_ = os.WriteFile(marker, nil, 0o600)
	}
	return "hint: the age identity is at " + path + ", the old location; it keeps working. To keep it with routes.json and apps.json in " + dir + ", move the identity*.txt files there yourself (nothing is moved automatically)."
}
