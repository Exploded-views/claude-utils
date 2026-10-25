package sync

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// ErrLocalOnlyTracked means the fetched tree tracks a per-PC path (config.json,
// denylist.txt, .state/..., ...). Merging it would overwrite this host's own
// file, and config.json can name a command to run, so the pull is refused
// before anything is merged.
var ErrLocalOnlyTracked = errors.New("pull refused")

const pullRefusedFile = "pull-refused"

func pullRefusedPath(dir string) string {
	return filepath.Join(dir, manifestStateDir, pullRefusedFile)
}

// refuseLocalOnly checks origin/<branch>'s tree after a fetch. On a hit it
// records the reason in .state/pull-refused (for `status`) and returns an
// error wrapping ErrLocalOnlyTracked; otherwise it clears that record.
func refuseLocalOnly(repo *gitwt.Repo, branch string) error {
	bad, err := repo.TrackedLocalOnly("refs/remotes/origin/" + branch)
	if err != nil {
		return nil // no remote branch yet: nothing to refuse
	}
	if len(bad) == 0 {
		_ = os.Remove(pullRefusedPath(repo.Dir))
		return nil
	}
	if len(bad) > 3 {
		bad = append(bad[:3], "...")
	}
	msg := fmt.Sprintf("the remote tracks per-PC files (%s); nothing is merged until it stops (remove them upstream)", strings.Join(bad, ", "))
	_ = writeSynced(pullRefusedPath(repo.Dir), []byte(msg+"\n"))
	return fmt.Errorf("%w: %s", ErrLocalOnlyTracked, msg)
}

// PullRefusedLine is the `status` line while a pull is refused, or "".
func PullRefusedLine(dir string) string {
	b, err := os.ReadFile(pullRefusedPath(dir))
	if err != nil {
		return ""
	}
	return "syncing paused, pull refused: " + strings.TrimSpace(string(b))
}
