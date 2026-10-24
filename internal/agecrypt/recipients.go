package agecrypt

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// canonLine is the key part of a recipients line: an ssh key without its comment.
func canonLine(line string) string {
	f := strings.Fields(line)
	if len(f) >= 2 && strings.HasPrefix(f[0], "ssh-") {
		return f[0] + " " + f[1]
	}
	if len(f) >= 1 {
		return f[0]
	}
	return ""
}

func isKeyLine(line string) bool {
	t := strings.TrimSpace(line)
	return t != "" && !strings.HasPrefix(t, "#")
}

// ListRecipients returns the recipient lines of the clone's .age-recipients.
func ListRecipients(dir string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(dir, RecipientsFile))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range strings.Split(string(data), "\n") {
		if isKeyLine(l) {
			out = append(out, strings.TrimSpace(l))
		}
	}
	return out, nil
}

// AcceptRecipients pins the work-tree's recipients after a person reviewed them.
func AcceptRecipients(repo *gitwt.Repo) error { return PinCurrent(repo) }

// AddRecipient adds key to .age-recipients, re-encrypts every file to the new
// set and commits (neutral message). The new key can read what this commit and
// later ones hold; older history stays unreadable to it. Nothing is pushed.
func AddRecipient(repo *gitwt.Repo, key string) error {
	key = strings.TrimSpace(key)
	if _, err := ParseRecipients([]byte(key + "\n")); err != nil || !isKeyLine(key) {
		return fmt.Errorf("not a valid age or ssh public key: %q", key)
	}
	cur, err := ListRecipients(repo.Dir)
	if err != nil {
		return err
	}
	for _, l := range cur {
		if canonLine(l) == canonLine(key) {
			return errors.New("that recipient is already listed")
		}
	}
	return rewriteRecipients(repo, append(cur, key), "memsync: recipients updated")
}

// RemoveRecipient removes the recipient whose key equals (or whose line
// contains, for a comment) key, re-encrypts and commits. The removed key still
// reads everything it has already fetched; only new blobs are closed to it.
func RemoveRecipient(repo *gitwt.Repo, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("no recipient given")
	}
	cur, err := ListRecipients(repo.Dir)
	if err != nil {
		return err
	}
	var keep []string
	removed := 0
	for _, l := range cur {
		if canonLine(l) == canonLine(key) || (strings.Contains(l, "#") && strings.Contains(l, key)) {
			removed++
			continue
		}
		keep = append(keep, l)
	}
	switch {
	case removed == 0:
		return errors.New("no such recipient")
	case len(keep) == 0:
		return errors.New("refusing to remove the last recipient")
	}
	return rewriteRecipients(repo, keep, "memsync: recipients updated")
}

func rewriteRecipients(repo *gitwt.Repo, keys []string, msg string) error {
	if err := CheckKeyed(repo); err != nil {
		return err
	}
	if u, _ := Unreviewed(repo); u {
		return ErrRecipientsUnreviewed
	}
	if err := WriteRecipients(repo.Dir, keys); err != nil {
		return err
	}
	if err := PinCurrent(repo); err != nil {
		return err
	}
	f, err := NewFilter(repo.Dir)
	if err != nil {
		return err
	}
	_ = f.ClearCache()
	os.Setenv(ReencryptEnv, "1")
	defer os.Unsetenv(ReencryptEnv)
	if _, err := repo.Run("add", "--renormalize", "."); err != nil {
		return fmt.Errorf("re-encrypt: %w", err)
	}
	if _, err := repo.Commit(msg); err != nil {
		return err
	}
	return nil
}
