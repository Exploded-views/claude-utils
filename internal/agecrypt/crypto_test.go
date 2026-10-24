package agecrypt

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// newIdentity generates a fresh identity file in dir and returns its path and
// public recipient. Keys are made inside the test; nothing is read from HOME.
func newIdentity(t *testing.T, name string) (string, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	rcpt, err := GenerateIdentity(p)
	if err != nil {
		t.Fatal(err)
	}
	return p, rcpt
}

func mustRecipients(t *testing.T, lines ...string) *Recipients {
	t.Helper()
	r, err := ParseRecipients([]byte(strings.Join(lines, "\n") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestGenerateIdentity_PrivateAndPublicOnly(t *testing.T) {
	p, rcpt := newIdentity(t, "id")
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("identity file mode: %v %v", fi, err)
	}
	if !strings.HasPrefix(rcpt, "age1") || strings.Contains(rcpt, "SECRET") {
		t.Fatalf("recipient should be the public key, got %q", rcpt)
	}
	if _, err := GenerateIdentity(p); err == nil {
		t.Error("must not overwrite an existing identity")
	}
	got, err := IdentityRecipient(p)
	if err != nil || got != rcpt {
		t.Errorf("IdentityRecipient = %q, %v; want %q", got, err, rcpt)
	}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	idPath, rcpt := newIdentity(t, "id")
	ids, err := LoadIdentities(idPath)
	if err != nil {
		t.Fatal(err)
	}
	ct, err := Encrypt([]byte("hello memory\n"), mustRecipients(t, rcpt))
	if err != nil {
		t.Fatal(err)
	}
	if !IsCiphertext(ct) || bytes.Contains(ct, []byte("hello")) {
		t.Fatal("not ciphertext")
	}
	pt, err := Decrypt(ct, ids)
	if err != nil || string(pt) != "hello memory\n" {
		t.Fatalf("decrypt = %q, %v", pt, err)
	}
	other, _ := newIdentity(t, "other")
	oids, _ := LoadIdentities(other)
	if _, err := Decrypt(ct, oids); err == nil {
		t.Error("a stranger's key must not decrypt")
	}
	if IsCiphertext([]byte("plain")) || IsCiphertext(nil) {
		t.Error("IsCiphertext false positive")
	}
}

func TestEncrypt_NoRecipientsRefuses(t *testing.T) {
	if _, err := Encrypt([]byte("x"), mustRecipients(t, "# only a comment")); err == nil {
		t.Error("encrypting to nobody must fail, not store plaintext")
	}
}

func TestRecipientsFingerprint(t *testing.T) {
	_, a := newIdentity(t, "a")
	_, b := newIdentity(t, "b")
	f1 := mustRecipients(t, a, b).Fingerprint()
	f2 := mustRecipients(t, "# note", b, "", a).Fingerprint()
	if f1 != f2 {
		t.Error("order and comments must not change the fingerprint")
	}
	if f1 == mustRecipients(t, a).Fingerprint() {
		t.Error("different sets must differ")
	}
}

func TestSSHIdentityAndRecipient(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	blk, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	sp := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(sp, pem.EncodeToMemory(blk), 0o600); err != nil {
		t.Fatal(err)
	}
	sshPub, _ := ssh.NewPublicKey(pub)
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " me@host"
	ct, err := Encrypt([]byte("via ssh"), mustRecipients(t, line))
	if err != nil {
		t.Fatal(err)
	}
	ids, err := LoadIdentities(sp)
	if err != nil {
		t.Fatal(err)
	}
	if pt, err := Decrypt(ct, ids); err != nil || string(pt) != "via ssh" {
		t.Fatalf("%q %v", pt, err)
	}
}

func TestLoadIdentities_Errors(t *testing.T) {
	if _, err := LoadIdentities(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing identity must fail")
	}
	p := filepath.Join(t.TempDir(), "junk")
	os.WriteFile(p, []byte("not a key\n"), 0o600)
	if _, err := LoadIdentities(p); err == nil {
		t.Error("junk identity must fail")
	}
}

func TestIsCiphertext_RequiresStanzaAndMAC(t *testing.T) {
	_, rcpt := newIdentity(t, "id")
	ct, _ := Encrypt([]byte("x"), mustRecipients(t, rcpt))
	if !IsCiphertext(ct) {
		t.Fatal("real ciphertext must be detected")
	}
	for name, b := range map[string]string{
		"prefix only":        Header + "\nbogus",
		"no mac":             Header + "\n-> X25519 abc\nxyz\n",
		"no stanza":          Header + "\n--- mac\n",
		"header w/o newline": Header,
	} {
		if IsCiphertext([]byte(b)) {
			t.Errorf("%s must not count as ciphertext", name)
		}
	}
}
