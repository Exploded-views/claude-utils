package agecrypt

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// ErrRecipientsUnreviewed means the work-tree's .age-recipients differs from
// the set this host pinned. Anyone with write access to the remote can change
// that file, so until a person accepts the change nothing is committed or
// pushed, and nothing is ever encrypted to the new keys.
var ErrRecipientsUnreviewed = errors.New("the recipients changed upstream and are not reviewed: nothing is committed or pushed (review with `claude-memsync recipients list`, then `recipients accept --yes`)")

const pinFile = "memage-pinned"

// pinPathFor is the pinned set's file, in the git dir (never committed).
func pinPathFor(gitCommonDir string) string { return filepath.Join(gitCommonDir, pinFile) }

func gitCommonDir(repo *gitwt.Repo) (string, error) {
	out, err := repo.Run("rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	d := strings.TrimSpace(out)
	if !filepath.IsAbs(d) {
		d = filepath.Join(repo.Dir, d)
	}
	return d, nil
}

func (r *Recipients) canonText() string {
	c := append([]string(nil), r.canon...)
	sort.Strings(c)
	return strings.Join(c, "\n") + "\n"
}

// PinCurrent pins the recipient set in the work-tree's .age-recipients. Init
// calls it, and so do `recipients add|remove|accept`: the pin changes only by a
// decision on this host.
func PinCurrent(repo *gitwt.Repo) error {
	data, err := os.ReadFile(filepath.Join(repo.Dir, RecipientsFile))
	if err != nil {
		return err
	}
	r, err := ParseRecipients(data)
	if err != nil {
		return err
	}
	if r.Len() == 0 {
		return errors.New("refusing to pin an empty recipient set")
	}
	d, err := gitCommonDir(repo)
	if err != nil {
		return err
	}
	tmp := pinPathFor(d) + ".tmp"
	if err := os.WriteFile(tmp, []byte(r.canonText()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, pinPathFor(d))
}

func readPin(repo *gitwt.Repo) (*Recipients, error) {
	d, err := gitCommonDir(repo)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(pinPathFor(d))
	if err != nil {
		return nil, err
	}
	return ParseRecipients(data)
}

// Unreviewed reports whether encryption is on and the work-tree's recipients
// differ from the pinned set (or no set is pinned). It returns the fingerprint
// of the work-tree set for logging.
func Unreviewed(repo *gitwt.Repo) (bool, string) {
	if !Enabled(repo) {
		return false, ""
	}
	pin, err := readPin(repo)
	if err != nil {
		return true, "unpinned"
	}
	data, err := os.ReadFile(filepath.Join(repo.Dir, RecipientsFile))
	if err != nil {
		return true, "missing"
	}
	cur, err := ParseRecipients(data)
	if err != nil {
		return true, "invalid"
	}
	return cur.Fingerprint() != pin.Fingerprint(), cur.Fingerprint()
}

func keyPrint(canon string) string {
	sum := sha256.Sum256([]byte(canon))
	return hex.EncodeToString(sum[:6])
}

// RecipientsDiff lists, as short fingerprints only, the keys in the work-tree
// that are not pinned and the pinned keys that are gone from it.
func RecipientsDiff(repo *gitwt.Repo) (added, removed []string, err error) {
	pin, err := readPin(repo)
	if err != nil {
		return nil, nil, fmt.Errorf("no pinned recipients: %w", err)
	}
	data, err := os.ReadFile(filepath.Join(repo.Dir, RecipientsFile))
	if err != nil {
		return nil, nil, err
	}
	cur, err := ParseRecipients(data)
	if err != nil {
		return nil, nil, err
	}
	in := func(l []string, s string) bool {
		for _, x := range l {
			if x == s {
				return true
			}
		}
		return false
	}
	for _, c := range cur.canon {
		if !in(pin.canon, c) {
			added = append(added, keyPrint(c))
		}
	}
	for _, c := range pin.canon {
		if !in(cur.canon, c) {
			removed = append(removed, keyPrint(c))
		}
	}
	return added, removed, nil
}

// PinnedFingerprints lists the short fingerprint of each pinned key.
func PinnedFingerprints(repo *gitwt.Repo) []string {
	pin, err := readPin(repo)
	if err != nil {
		return nil
	}
	var out []string
	for _, c := range pin.canon {
		out = append(out, keyPrint(c))
	}
	return out
}
