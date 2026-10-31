package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/config"
)

// A repo the hourly recheck found under another owner is held; status must
// say so, with the time the hold began.
func TestVisibilityLine_NamesTheOwnerHold(t *testing.T) {
	dir := t.TempDir()
	st := filepath.Join(dir, ".state")
	if err := os.MkdirAll(st, 0o700); err != nil {
		t.Fatal(err)
	}
	state := "held since 2026-10-31T10:00:00Z: repo moved to Other/mem-personal: update routes.json and the remote\n"
	if err := os.WriteFile(filepath.Join(st, "visibility"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	got := visibilityLine(config.Config{PrivateOnly: true, SyncDir: dir})
	if !strings.HasPrefix(got, "pushing stopped: ") {
		t.Errorf("status line %q must start with %q", got, "pushing stopped: ")
	}
	for _, want := range []string{"held: repo now belongs to another owner", "since 2026-10-31T10:00:00Z", "commits stay local"} {
		if !strings.Contains(got, want) {
			t.Errorf("status line %q lacks %q", got, want)
		}
	}
}
