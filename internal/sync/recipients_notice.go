package sync

import (
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

const recipientsStateFile = "recipients-unreviewed"

func recipientsStatePath(dir string) string {
	return filepath.Join(dir, manifestStateDir, recipientsStateFile)
}

// noteRecipientsChange is logUnreviewed for after a pull; kept as the one
// place the loop calls.
func noteRecipientsChange(repo *gitwt.Repo, _, _ string) { logUnreviewed(repo) }

// logUnreviewed logs once per change of state when the work-tree's recipients
// differ from the pinned set (anyone with write access to the remote can edit
// them). While they differ the daemon commits and pushes nothing.
func logUnreviewed(repo *gitwt.Repo) bool {
	p := recipientsStatePath(repo.Dir)
	u, fp := agecrypt.Unreviewed(repo)
	if !u {
		_ = os.Remove(p)
		return false
	}
	if b, err := os.ReadFile(p); err != nil || strings.TrimSpace(string(b)) != fp {
		log.Println("WARNING: " + agecrypt.ErrRecipientsUnreviewed.Error())
		_ = writeSynced(p, []byte(fp+"\n"))
	}
	return true
}
