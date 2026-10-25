package agecrypt

import (
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"filippo.io/age"

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
	return pushGuard(repo, branch, true)
}

// PushGuardJoinRequest is PushGuard without the recipients-review gate, for
// pushing a join request: that commit encrypts nothing, so an unreviewed
// recipient set cannot leak through it, and a host waiting to be admitted has
// by definition not reviewed the set that will include it. Every blob is still
// checked: only valid join requests may be plaintext.
func PushGuardJoinRequest(repo *gitwt.Repo, branch string) error {
	return pushGuard(repo, branch, false)
}

func pushGuard(repo *gitwt.Repo, branch string, requireReviewed bool) error {
	if !Enabled(repo) {
		return nil
	}
	if u, _ := Unreviewed(repo); u && requireReviewed {
		return ErrRecipientsUnreviewed
	}
	// What gets pushed is the branch, not whatever HEAD is.
	head := "refs/heads/" + branch
	if _, err := repo.RunClean("rev-parse", "--verify", "--quiet", head); err != nil {
		return nil // nothing to push
	}
	rng := head
	if _, err := repo.RunClean("rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch); err == nil {
		rng = "refs/remotes/origin/" + branch + ".." + head
	}
	return guardRange(repo, rng, SchemeOf(repo) == SchemePQ)
}

// PushGuardCommit is the push guard for a commit that is built but not yet the
// branch tip: every blob that origin/<branch>..commit adds or changes (all of
// them, for a root commit) must be hybrid ciphertext. rekey-pq runs it before
// it moves HEAD, so a refusal leaves the repo untouched.
func PushGuardCommit(repo *gitwt.Repo, branch, commit string) error {
	rng := commit
	if _, err := repo.RunClean("rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch); err == nil {
		rng = "refs/remotes/origin/" + branch + ".." + commit
	}
	return guardRange(repo, rng, true)
}

func guardRange(repo *gitwt.Repo, rng string, pq bool) error {
	out, err := repo.RunClean("-c", "core.quotePath=true", "-c", "log.showRoot=true", "-c", "diff.renames=false",
		"-c", "log.showSignature=false", "-c", "core.fsmonitor=false",
		"log", "--root", "-m", "--raw", "--no-abbrev", "--no-renames", "--no-notes", "--no-ext-diff", "--no-textconv", "--format=", rng)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	var oids, paths []string
	var joinOIDs, joinPaths []string
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
		if IsJoinPath(path) {
			// Plaintext is allowed here, but only a strictly valid request.
			joinOIDs = append(joinOIDs, f[3])
			joinPaths = append(joinPaths, path)
			continue
		}
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
	jb, err := repo.BlobContents(joinOIDs)
	if err != nil {
		return err
	}
	for i, b := range jb {
		if !ValidJoinBlob(joinPaths[i], b) {
			bad++
		}
	}
	for i, b := range blobs {
		if !IsCiphertext(b) || (pq && !IsHybridCiphertext(b)) {
			bad++
			_ = paths[i] // the name is not logged: it may be sensitive
		}
	}
	if bad > 0 {
		return fmt.Errorf("push refused: %d blob(s) to push are not age ciphertext of the repo's scheme (is the memage filter configured in this clone?)", bad)
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

// PlainReader returns a function that turns a blob read straight from git into
// plaintext. For an unencrypted repo it returns the blob as is. For an
// encrypted one it decrypts age blobs with this host's identity and returns an
// error when it cannot. Plaintext blobs pass through.
func PlainReader(repo *gitwt.Repo) func([]byte) ([]byte, error) {
	if !Enabled(repo) {
		return func(b []byte) ([]byte, error) { return b, nil }
	}
	var ids []age.Identity
	var idErr error
	loaded := false
	return func(b []byte) ([]byte, error) {
		if !IsCiphertext(b) {
			return b, nil
		}
		if !loaded {
			ids, idErr = LoadIdentities(IdentityPath(repo))
			loaded = true
		}
		if idErr != nil {
			return nil, idErr
		}
		return Decrypt(b, ids)
	}
}
