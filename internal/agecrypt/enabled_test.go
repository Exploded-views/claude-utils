package agecrypt

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

func plainRepo(t *testing.T) *gitwt.Repo {
	t.Helper()
	gitEnv(t)
	r := gitwt.New(t.TempDir())
	if err := r.Init("main"); err != nil {
		t.Fatal(err)
	}
	return r
}

func commitFile(t *testing.T, r *gitwt.Repo, rel, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.Dir, rel), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.AddAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("c " + rel); err != nil {
		t.Fatal(err)
	}
}

// EnabledKnowing agrees with Enabled, and trusts a caller's answer about HEAD
// instead of asking git.
func TestEnabledKnowing(t *testing.T) {
	never := func() (bool, bool) { t.Fatal("asked the caller after a cheaper test settled it"); return false, false }
	unknown := func() (bool, bool) { return false, false }

	r := plainRepo(t)
	commitFile(t, r, "note.md", "x\n")
	if EnabledKnowing(r, unknown) || Enabled(r) {
		t.Error("a plain repo counted as encrypted")
	}
	// The caller knows HEAD has no recipients file, or has one.
	if EnabledKnowing(r, func() (bool, bool) { return false, true }) {
		t.Error("known absent at HEAD, but reported enabled")
	}
	if !EnabledKnowing(r, func() (bool, bool) { return true, true }) {
		t.Error("known present at HEAD, but reported disabled")
	}

	// Filter configured: no further question is asked.
	c := plainRepo(t)
	if err := c.ConfigSet("filter."+FilterName+".process", "x process"); err != nil {
		t.Fatal(err)
	}
	if !EnabledKnowing(c, never) {
		t.Error("a configured filter was not enough")
	}

	// A recipients file in the work-tree: same.
	w := plainRepo(t)
	if err := os.WriteFile(filepath.Join(w.Dir, RecipientsFile), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !EnabledKnowing(w, never) {
		t.Error("a recipients file in the work-tree was not enough")
	}

	// Recipients only in HEAD, caller does not know: git is asked.
	h := plainRepo(t)
	commitFile(t, h, RecipientsFile, "x\n")
	if err := os.Remove(filepath.Join(h.Dir, RecipientsFile)); err != nil {
		t.Fatal(err)
	}
	if !EnabledKnowing(h, unknown) {
		t.Error("recipients at HEAD were not found through git")
	}
}

func TestPlainReaderPassesPlaintextAndDecryptsForAnEnabledRepo(t *testing.T) {
	r := plainRepo(t)
	pass := PlainReader(r)
	if got, err := pass([]byte("hello")); err != nil || string(got) != "hello" {
		t.Fatalf("plain repo: %q %v", got, err)
	}

	idPath, rcpt := newIdentity(t, "id")
	rs := mustRecipients(t, rcpt)
	ct, err := Encrypt([]byte("secret note\n"), rs)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ConfigSet(IdentityConfigKey, idPath); err != nil {
		t.Fatal(err)
	}
	read := PlainReaderFor(r, true)
	if got, err := read(ct); err != nil || string(got) != "secret note\n" {
		t.Errorf("decrypt: %q %v", got, err)
	}
	if got, err := read([]byte("not ciphertext")); err != nil || string(got) != "not ciphertext" {
		t.Errorf("plaintext in an encrypted repo must pass through: %q %v", got, err)
	}

	// Ciphertext for someone else cannot be read.
	_, other := newIdentity(t, "other")
	foreign, _ := Encrypt([]byte("x"), mustRecipients(t, other))
	if got, err := read(foreign); err == nil {
		t.Errorf("decrypted a note for another recipient: %q", got)
	}

	// No identity configured: an error, not plaintext.
	bare := plainRepo(t)
	if got, err := PlainReaderFor(bare, true)(ct); err == nil {
		t.Errorf("read ciphertext with no identity: %q", got)
	}
	// PlainReader asks Enabled itself.
	if err := os.WriteFile(filepath.Join(bare.Dir, RecipientsFile), []byte(rcpt+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PlainReader(bare)(ct); err == nil {
		t.Error("PlainReader on an enabled repo without an identity returned plaintext")
	}
}

func TestRecipientsChanged(t *testing.T) {
	r := plainRepo(t)
	commitFile(t, r, RecipientsFile, "one\n")
	commitFile(t, r, "other.md", "o\n")
	commitFile(t, r, RecipientsFile, "one\ntwo\n")
	if !RecipientsChanged(r, "HEAD~2", "HEAD") {
		t.Error("an edited recipients file was not seen")
	}
	if RecipientsChanged(r, "HEAD~1", "HEAD~1") {
		t.Error("identical revs reported as changed")
	}
	if RecipientsChanged(r, "HEAD~2", "HEAD~1") {
		t.Error("an unrelated commit reported as a recipients change")
	}
}
