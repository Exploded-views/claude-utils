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
//
// Anything committed or pulled under an older set is re-encrypted to the new
// one (and committed) before the next push, so no blob a newly admitted host
// cannot read goes out after the accept. A host that cannot read the repo
// itself only pins; it has nothing to re-encrypt.
func AcceptRecipients(repo *gitwt.Repo) error {
	if err := PinCurrent(repo); err != nil {
		return err
	}
	if CheckKeyed(repo) != nil || !repo.HasHead() {
		return nil
	}
	return reencryptHead(repo, "memsync: recipients updated")
}

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
	return reencryptHead(repo, msg)
}

// reencryptHead rewrites every encrypted file of HEAD to the pinned recipient
// set (and records the work-tree's .age-recipients) in one new commit. The
// commit is built from HEAD alone, in a private index, with a compare-and-swap
// ref update: whatever else is staged in the real index (an interrupted cycle's
// add, the user's own work) is never committed. Afterwards only the paths this
// commit touched are updated in the real index, and only where the entry still
// equals HEAD's; other staged entries are left as they are.
func reencryptHead(repo *gitwt.Repo, msg string) error {
	if !repo.HasHead() {
		return nil // nothing committed yet: the first cycle commits the recipients
	}
	oldRev, err := repo.Run("rev-parse", "HEAD")
	if err != nil {
		return err
	}
	oldRev = strings.TrimSpace(oldRev)
	pin, err := readPin(repo)
	if err != nil {
		return err
	}
	ids, err := LoadIdentities(IdentityPath(repo))
	if err != nil {
		return err
	}
	out, err := repo.Run("ls-tree", "-r", "-z", "HEAD")
	if err != nil {
		return err
	}
	type ent struct{ mode, oid, path string }
	var ents []ent
	var oids []string
	for _, rec := range strings.Split(out, "\x00") {
		tab := strings.IndexByte(rec, '\t')
		if tab < 0 {
			continue
		}
		f := strings.Fields(rec[:tab])
		path := rec[tab+1:]
		if len(f) != 3 || f[1] != "blob" || (f[0] != "100644" && f[0] != "100755") || Exempt(path) {
			continue
		}
		ents = append(ents, ent{f[0], f[2], path})
		oids = append(oids, f[2])
	}
	blobs, err := repo.BlobContents(oids)
	if err != nil {
		return err
	}
	headOID := map[string]string{} // path -> HEAD's blob id, for the real-index update
	var info strings.Builder
	for i, b := range blobs {
		pt := b
		if IsCiphertext(b) {
			if pt, err = Decrypt(b, ids); err != nil {
				return fmt.Errorf("re-encrypt: cannot read a file of this repo (%w)", err)
			}
		}
		ct, err := Encrypt(pt, pin)
		if err != nil {
			return err
		}
		oid, err := repo.RunStdin(ct, "hash-object", "-w", "--no-filters", "--stdin")
		if err != nil {
			return err
		}
		e := ents[i]
		headOID[e.path] = e.oid
		fmt.Fprintf(&info, "%s %s\t%s\x00", e.mode, strings.TrimSpace(oid), e.path)
	}
	if data, err := os.ReadFile(filepath.Join(repo.Dir, RecipientsFile)); err == nil {
		oid, err := repo.RunStdin(data, "hash-object", "-w", "--no-filters", "--stdin")
		if err != nil {
			return err
		}
		if prev, perr := repo.Run("rev-parse", "--verify", "--quiet", "HEAD:"+RecipientsFile); perr == nil {
			headOID[RecipientsFile] = strings.TrimSpace(prev)
		}
		fmt.Fprintf(&info, "100644 %s\t%s\x00", strings.TrimSpace(oid), RecipientsFile)
	}
	gitDir, err := gitCommonDir(repo)
	if err != nil {
		return err
	}
	tmpIdx := filepath.Join(gitDir, fmt.Sprintf("memage-index-%d", os.Getpid()))
	defer os.Remove(tmpIdx)
	env := []string{"GIT_INDEX_FILE=" + tmpIdx}
	if _, err := repo.RunEnv(env, nil, "read-tree", "HEAD"); err != nil {
		return err
	}
	if _, err := repo.RunEnv(env, []byte(info.String()), "update-index", "-z", "--index-info"); err != nil {
		return fmt.Errorf("re-encrypt: %w", err)
	}
	tree, err := repo.RunEnv(env, nil, "write-tree")
	if err != nil {
		return err
	}
	commit, err := repo.Run("commit-tree", strings.TrimSpace(tree), "-p", oldRev, "-m", msg)
	if err != nil {
		return err
	}
	commit = strings.TrimSpace(commit)
	if _, err := repo.Run("update-ref", "-m", msg, "HEAD", commit, oldRev); err != nil {
		return fmt.Errorf("re-encrypt: HEAD moved underneath (is the daemon running?): %w", err)
	}
	// Bring the real index along for the paths whose entry still equals HEAD's
	// old one (or is absent); anything the user staged differently stays.
	cur, err := repo.Run("ls-files", "-s", "-z")
	if err != nil {
		return err
	}
	have := map[string]string{}
	for _, rec := range strings.Split(cur, "\x00") {
		tab := strings.IndexByte(rec, '\t')
		if tab < 0 {
			continue
		}
		if f := strings.Fields(rec[:tab]); len(f) == 3 && f[2] == "0" {
			have[rec[tab+1:]] = f[1]
		}
	}
	var upd strings.Builder
	for _, rec := range strings.Split(info.String(), "\x00") {
		tab := strings.IndexByte(rec, '\t')
		if tab < 0 {
			continue
		}
		path := rec[tab+1:]
		if got, ok := have[path]; ok && got != headOID[path] {
			continue // staged differently: not ours to touch
		}
		fmt.Fprintf(&upd, "%s\x00", rec)
	}
	if upd.Len() > 0 {
		if _, err := repo.RunStdin([]byte(upd.String()), "update-index", "-z", "--index-info"); err != nil {
			return fmt.Errorf("re-encrypt: update index: %w", err)
		}
	}
	return nil
}
