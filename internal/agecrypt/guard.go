package agecrypt

import (
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// ErrKeyless means this host's identity cannot decrypt what the repo holds.
var ErrKeyless = errors.New("this host's age identity cannot decrypt the repo (add its public key with `claude-memsync recipients add` from a host that can)")

// CheckKeyed returns ErrKeyless (wrapped) when encryption is on and the
// identity cannot decrypt a note in HEAD (MEMORY.md files first, at most 50
// notes). It returns nil when encryption is off or HEAD holds no encrypted file.
func CheckKeyed(repo *gitwt.Repo) error {
	if !Enabled(repo) {
		return nil
	}
	if !Configured(repo) {
		return fmt.Errorf("%w: the age filter is not configured in this clone", ErrKeyless)
	}
	if !repo.HasHead() {
		return nil
	}
	out, err := repo.Run("ls-tree", "-r", "-z", "HEAD")
	if err != nil {
		return err
	}
	type ent struct{ path, oid string }
	var ents []ent
	for _, rec := range strings.Split(out, "\x00") {
		tab := strings.IndexByte(rec, '\t')
		if tab < 0 {
			continue
		}
		f := strings.Fields(rec[:tab])
		path := rec[tab+1:]
		if len(f) != 3 || f[1] != "blob" || Exempt(path) {
			continue
		}
		ents = append(ents, ent{path, f[2]})
	}
	sort.SliceStable(ents, func(i, j int) bool {
		return strings.HasSuffix(ents[i].path, "MEMORY.md") && !strings.HasSuffix(ents[j].path, "MEMORY.md")
	})
	if len(ents) > 50 {
		ents = ents[:50]
	}
	if len(ents) == 0 {
		return nil
	}
	oids := make([]string, len(ents))
	for i, e := range ents {
		oids[i] = e.oid
	}
	blobs, err := repo.BlobContents(oids)
	if err != nil {
		return err
	}
	ids, idErr := LoadIdentities(IdentityPath(repo))
	for _, b := range blobs {
		if !IsCiphertext(b) {
			continue
		}
		if idErr != nil {
			return fmt.Errorf("%w: %v", ErrKeyless, idErr)
		}
		if _, err := Decrypt(b, ids); err != nil {
			return fmt.Errorf("%w: %v", ErrKeyless, err)
		}
	}
	return nil
}

// PushGuard refuses a push that would send a plaintext blob. Every blob that a
// commit in origin/<branch>..HEAD adds or changes, outside the exempt files,
// must be age ciphertext. It catches a clone whose filter config went missing.
// It returns nil when encryption is off.
func PushGuard(repo *gitwt.Repo, branch string) error {
	if !Enabled(repo) || !repo.HasHead() {
		return nil
	}
	rng := "HEAD"
	if _, err := repo.Run("rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch); err == nil {
		rng = "refs/remotes/origin/" + branch + "..HEAD"
	}
	out, err := repo.Run("-c", "core.quotePath=true", "log", "-m", "--raw", "--no-abbrev", "--no-renames", "--format=", rng)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	var oids, paths []string
	for _, line := range strings.Split(out, "\n") {
		tab := strings.IndexByte(line, '\t')
		if !strings.HasPrefix(line, ":") || tab < 0 {
			continue
		}
		f := strings.Fields(line[:tab])
		if len(f) != 5 || f[4] == "D" || f[1] == "160000" || seen[f[3]+line[tab:]] {
			continue
		}
		seen[f[3]+line[tab:]] = true
		path := line[tab+1:]
		if Exempt(path) {
			continue
		}
		oids = append(oids, f[3])
		paths = append(paths, path)
	}
	blobs, err := repo.BlobContents(oids)
	if err != nil {
		return err
	}
	var bad int
	for i, b := range blobs {
		if !IsCiphertext(b) {
			bad++
			_ = paths[i] // the name is not logged: it may be sensitive
		}
	}
	if bad > 0 {
		return fmt.Errorf("push refused: %d blob(s) to push are not age ciphertext (is the memage filter configured in this clone?)", bad)
	}
	return nil
}

// RecipientsChanged reports whether .age-recipients differs between two revs.
func RecipientsChanged(repo *gitwt.Repo, pre, post string) bool {
	_, err := repo.Run("diff", "--quiet", pre, post, "--", RecipientsFile)
	if err == nil {
		return false
	}
	var ee *exec.ExitError
	return errors.As(err, &ee) // exit 1 = differs
}
