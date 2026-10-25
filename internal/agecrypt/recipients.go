package agecrypt

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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

// CanonLine is the key part of a recipients line, as the pin records it.
func CanonLine(line string) string { return canonLine(line) }

// stripTrailingComment drops a " # ..." comment from an age key line.
func stripTrailingComment(line string) string {
	if i := strings.Index(line, " #"); i >= 0 {
		return strings.TrimSpace(line[:i])
	}
	if i := strings.Index(line, "\t#"); i >= 0 {
		return strings.TrimSpace(line[:i])
	}
	return line
}

var labelRE = regexp.MustCompile(`(?:^|\s)#\s*host:([a-z0-9][a-z0-9-]{0,31})\s*$`)

// LabelOf is the host label a recipients line carries in a trailing comment
// "# host:<label>", or "" for a line without one.
func LabelOf(line string) string {
	if m := labelRE.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
		return m[1]
	}
	return ""
}

// ListLabels returns the sorted, distinct host labels of the work-tree's
// recipients file.
func ListLabels(dir string) ([]string, error) {
	lines, err := ListRecipients(dir)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, l := range lines {
		if lb := LabelOf(l); lb != "" && !seen[lb] {
			seen[lb] = true
			out = append(out, lb)
		}
	}
	sort.Strings(out)
	return out, nil
}

// AddRecipientLabelled is AddRecipient with the host's label recorded on the
// line as a trailing comment "# host:<label>", so the host can later be found
// and retired by name.
func AddRecipientLabelled(repo *gitwt.Repo, key, label string) error {
	if label == "" {
		return AddRecipient(repo, key)
	}
	if !labelRE.MatchString("# host:" + label) {
		return fmt.Errorf("invalid host label %q (use 1-32 characters of a-z, 0-9 and '-')", label)
	}
	if cur, err := ListRecipients(repo.Dir); err == nil {
		for _, l := range cur {
			if LabelOf(l) == label && canonLine(l) != canonLine(key) {
				return fmt.Errorf("host label %q is already bound to another recipient", label)
			}
		}
	}
	return AddRecipient(repo, strings.TrimSpace(key)+" # host:"+label)
}

// ErrLabelAmbiguous means a host label is bound to more than one key.
var ErrLabelAmbiguous = errors.New("that host label is bound to more than one key in the pinned recipients")

const hostComment = " # host:"

func labelSuffix(label string) string {
	if label == "" {
		return ""
	}
	return hostComment + label
}

// splitCanon splits a reviewed canonical entry into its key and its label.
func splitCanon(c string) (key, label string) {
	if i := strings.Index(c, hostComment); i >= 0 {
		return c[:i], c[i+len(hostComment):]
	}
	return c, ""
}

// KeyPrint is the short fingerprint of a recipient key (a canonical key as
// PinnedKeysForLabel returns it), safe to print.
func KeyPrint(key string) string { return keyPrint(key) }

// PinnedKeysForLabel resolves a host label through the pin, the set this host
// has reviewed, and never through the work-tree's comments, which anyone with
// write access to the remote can change. It returns the keys the pin binds to
// the label (none if it binds none) and ErrLabelAmbiguous if more than one.
func PinnedKeysForLabel(repo *gitwt.Repo, label string) ([]string, error) {
	pin, err := readPin(repo)
	if err != nil {
		return nil, fmt.Errorf("no pinned recipients: %w", err)
	}
	var keys []string
	for _, c := range pin.canon {
		if k, l := splitCanon(c); l == label && label != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) > 1 {
		return keys, ErrLabelAmbiguous
	}
	return keys, nil
}

// RemoveRecipientLabel removes the recipient the pin binds to label,
// re-encrypts HEAD to the rest and commits (as RemoveRecipient does). It
// refuses while the work-tree's recipients are unreviewed, and when the label
// is bound to more than one key. History stays readable to the removed key.
func RemoveRecipientLabel(repo *gitwt.Repo, label string) error {
	if u, _ := Unreviewed(repo); u {
		return ErrRecipientsUnreviewed
	}
	keys, err := PinnedKeysForLabel(repo, label)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return errors.New("no recipient with that label")
	}
	cur, err := ListRecipients(repo.Dir)
	if err != nil {
		return err
	}
	var keep []string
	removed := 0
	for _, l := range cur {
		if canonLine(l) == keys[0] {
			removed++
			continue
		}
		keep = append(keep, l)
	}
	switch {
	case removed == 0:
		return errors.New("no recipient with that label")
	case len(keep) == 0:
		return errors.New("refusing to remove the last recipient")
	}
	return rewriteRecipients(repo, keep, "memsync: recipients updated")
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
	return reencryptHeadDrop(repo, msg, nil)
}

// reencryptHeadDrop is reencryptHead that also removes the given paths from
// the commit's tree (and, when unchanged, from the work-tree).
func reencryptHeadDrop(repo *gitwt.Repo, msg string, drop []string) error {
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
	if err := addDrops(repo, &info, headOID, drop); err != nil {
		return err
	}
	return commitInfo(repo, oldRev, msg, info.String(), headOID, drop)
}

// addDrops adds index-info lines that delete each path HEAD holds.
func addDrops(repo *gitwt.Repo, info *strings.Builder, headOID map[string]string, drop []string) error {
	for _, d := range drop {
		oid, err := repo.Run("rev-parse", "--verify", "--quiet", "HEAD:"+d)
		if err != nil {
			continue // not in HEAD: nothing to drop
		}
		headOID[d] = strings.TrimSpace(oid)
		fmt.Fprintf(info, "0 %s\t%s\x00", strings.Repeat("0", 40), d)
	}
	return nil
}

// commitInfo builds one commit on oldRev from HEAD's tree with the given
// index-info applied, in a private index, with a compare-and-swap ref update;
// then brings the real index (and dropped work-tree files) along.
func commitInfo(repo *gitwt.Repo, oldRev, msg, infoText string, headOID map[string]string, drop []string) error {
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
	if _, err := repo.RunEnv(env, []byte(infoText), "update-index", "-z", "--index-info"); err != nil {
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
	for _, rec := range strings.Split(infoText, "\x00") {
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
	for _, d := range drop {
		// A dropped file the work-tree still holds, unchanged, goes too.
		fp := filepath.Join(repo.Dir, filepath.FromSlash(d))
		if want, ok := headOID[d]; ok {
			if got, err := repo.Run("hash-object", "--no-filters", "--", fp); err == nil && strings.TrimSpace(got) == want {
				_ = os.Remove(fp)
			}
		}
	}
	return nil
}

// LabelSelf puts "# host:<label>" on this host's own recipient line and
// commits it (re-encrypting to the same keys, re-pinning), so the host can be
// named later. It refuses a line that already carries another label and a
// label that another key holds. It does nothing if the line is already so
// labelled.
func LabelSelf(repo *gitwt.Repo, label string) error {
	if !labelRE.MatchString("# host:" + label) {
		return fmt.Errorf("invalid host label %q (use 1-32 characters of a-z, 0-9 and '-')", label)
	}
	own, err := IdentityRecipient(IdentityPath(repo))
	if err != nil {
		return err
	}
	cur, err := ListRecipients(repo.Dir)
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(cur))
	found, changed := false, false
	for _, l := range cur {
		switch {
		case canonLine(l) == own:
			found = true
			switch LabelOf(l) {
			case label:
			case "":
				l = strings.TrimSpace(l) + hostComment + label
				changed = true
			default:
				return fmt.Errorf("this host's key is already labelled %q", LabelOf(l))
			}
		case LabelOf(l) == label:
			return fmt.Errorf("host label %q is already bound to another recipient", label)
		}
		keys = append(keys, l)
	}
	if !found {
		return errors.New("this host's key is not a recipient of the repo")
	}
	if !changed {
		return nil
	}
	return rewriteRecipients(repo, keys, "memsync: recipients updated")
}
