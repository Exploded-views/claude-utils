package main

import (
	"os"
	"path/filepath"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/archive"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/related"
)

// ageOpener reads a related read clone, decrypting what this host's age
// identity can. A read clone is exported as the repo stores it, so an
// encrypted repo's notes are ciphertext beside a plaintext .age-recipients.
// IdentityPath "" means this host's default identity.
type ageOpener struct{ IdentityPath string }

func (o ageOpener) identity() string {
	if o.IdentityPath != "" {
		return o.IdentityPath
	}
	return defaultIdentityPath()
}

// Readable: a plaintext clone always; an encrypted one only when this host has
// an identity and, if it is an age key, it is one of the clone's recipients.
func (o ageOpener) Readable(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, related.EncryptedMarker)); err != nil {
		return true
	}
	if _, err := agecrypt.LoadIdentities(o.identity()); err != nil {
		return false
	}
	mine, err := agecrypt.IdentityRecipient(o.identity())
	if err != nil {
		return true // not an X25519 key: decryption decides
	}
	keys, err := agecrypt.ListRecipients(dir)
	if err != nil {
		return false
	}
	for _, k := range keys {
		if k == mine {
			return true
		}
	}
	return false
}

// ReadFile returns the plaintext of p: ciphertext is decrypted, anything else
// is returned as it is.
func (o ageOpener) ReadFile(p string) ([]byte, error) {
	b, err := os.ReadFile(p)
	if err != nil || !agecrypt.IsCiphertext(b) {
		return b, err
	}
	ids, err := agecrypt.LoadIdentities(o.identity())
	if err != nil {
		return nil, err
	}
	return agecrypt.Decrypt(b, ids)
}

// useArchiveReader points archive.Read at a reader that decrypts the repo's
// ciphertext with its configured identity, and returns the function that puts
// the old one back. An unencrypted repo reads as before.
func useArchiveReader(syncDir string) (restore func()) {
	old := archive.Read
	plain := agecrypt.PlainReader(gitwt.New(syncDir))
	archive.Read = func(p string) ([]byte, error) {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		return plain(b)
	}
	return func() { archive.Read = old }
}
