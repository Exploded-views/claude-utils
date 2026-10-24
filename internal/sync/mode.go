package sync

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/config"
)

// A sync dir is set up for one projectKey mode, path or remote, and stays in
// it: the two store the same folder in different places of the repo, and
// moving a PC from one to the other on the same sync dir has no safe answer
// for the notes it holds. init records the mode in .state/mode (per-PC, like
// the rest of .state/); the daemon and init --force refuse a different one.
// Switching means running init into a fresh sync dir.

func modePath(syncDir string) string { return filepath.Join(syncDir, manifestStateDir, "mode") }

// RecordMode writes the mode the sync dir is set up for, atomically.
func RecordMode(syncDir, mode string) error {
	if err := os.MkdirAll(filepath.Dir(modePath(syncDir)), 0700); err != nil {
		return err
	}
	return writeAtomic(modePath(syncDir), []byte(mode+"\n"))
}

// CheckMode returns an error unless want is the mode the sync dir was set up
// for. A sync dir with no record was made before modes were recorded, and those
// only ever knew path mode.
func CheckMode(syncDir, want string) error {
	got := config.ProjectKeyPath
	b, err := os.ReadFile(modePath(syncDir))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return fmt.Errorf("read %s: %w", modePath(syncDir), err)
	default:
		got = strings.TrimSpace(string(b))
		if got != config.ProjectKeyPath && got != config.ProjectKeyRemote {
			return fmt.Errorf("%s says %q, which is neither %q nor %q", modePath(syncDir), got, config.ProjectKeyPath, config.ProjectKeyRemote)
		}
	}
	if got != want {
		return fmt.Errorf("projectKey is %q but %s was set up for %q, and switching modes on an existing sync dir is not supported: "+
			"run `claude-memsync init --project-key %s --sync-dir <a fresh sync dir>` to start a fresh sync dir in that mode",
			want, syncDir, got, want)
	}
	return nil
}
