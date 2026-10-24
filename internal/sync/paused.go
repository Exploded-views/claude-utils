package sync

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// pausedFile is written to .state while syncing is paused because the user is
// resolving a rebase or merge by hand (errHandResolving), and removed by the
// first cycle that goes on. It holds when the pause began, so `claude-memsync
// status` can say so: the daemon is running but syncing nothing.
const pausedFile = "paused"

func pausedPath(dir string) string {
	return filepath.Join(dir, manifestStateDir, pausedFile)
}

// markPaused records that syncing is paused, unless it already says so (the
// time is when the pause began).
func markPaused(repo *gitwt.Repo) {
	if _, err := os.Stat(pausedPath(repo.Dir)); err == nil {
		return
	}
	_ = writeSynced(pausedPath(repo.Dir), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"))
}

func clearPaused(repo *gitwt.Repo) {
	_ = os.Remove(pausedPath(repo.Dir))
}

// PausedSince reports whether the daemon is paused for a rebase or merge the
// user is resolving in the sync repo in dir, and since when (zero if the file
// cannot be read).
func PausedSince(dir string) (time.Time, bool) {
	b, err := os.ReadFile(pausedPath(dir))
	if errors.Is(err, fs.ErrNotExist) || err != nil {
		return time.Time{}, false
	}
	t, _ := time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
	return t, true
}
