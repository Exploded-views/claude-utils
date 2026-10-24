package sync

import (
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

const recipientsNoticeFile = "recipients-changed"

func recipientsNoticePath(dir string) string {
	return filepath.Join(dir, manifestStateDir, recipientsNoticeFile)
}

// noteRecipientsChange warns when a pull changed .age-recipients: anyone with
// write access to the remote could have added their own key. The warning is
// logged and kept for `status` until someone runs `recipients list`.
func noteRecipientsChange(repo *gitwt.Repo, pre, post string) {
	if !agecrypt.Enabled(repo) || !agecrypt.RecipientsChanged(repo, pre, post) {
		return
	}
	log.Println("WARNING: .age-recipients changed upstream; review it with `claude-memsync recipients list` (anyone with write access to the remote can add a key)")
	_ = writeSynced(recipientsNoticePath(repo.Dir), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"))
}

// RecipientsNotice reports whether an unreviewed upstream recipients change is
// recorded for the sync repo in dir.
func RecipientsNotice(dir string) bool {
	_, err := os.Stat(recipientsNoticePath(dir))
	return err == nil
}

// ClearRecipientsNotice marks the recipients as reviewed.
func ClearRecipientsNotice(dir string) { _ = os.Remove(recipientsNoticePath(dir)) }
