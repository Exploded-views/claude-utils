package main

import (
	"filippo.io/age"

	"os"
	"path/filepath"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/archive"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/related"
)

// ageOpener reads a related read clone, decrypting what this host's age
// identities can. A read clone is exported as the repo stores it, so an
// encrypted repo's notes are ciphertext beside a plaintext .age-recipients.
// The identities are IdentityPath ("" means this host's default identity) and
// Extra, the identities configured on this host's own encrypted sync repos.
type ageOpener struct {
	IdentityPath string
	Extra        []string
}

func (o ageOpener) paths() []string {
	first := o.IdentityPath
	if first == "" {
		first = defaultIdentityPath()
	}
	out := []string{first}
	for _, p := range o.Extra {
		dup := false
		for _, q := range out {
			dup = dup || p == q
		}
		if !dup {
			out = append(out, p)
		}
	}
	return out
}

// Readable: a plaintext clone always; an encrypted one only when one of the
// identities loads and, if it is an age key, is one of the clone's recipients.
func (o ageOpener) Readable(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, related.EncryptedMarker)); err != nil {
		return true
	}
	keys, kerr := agecrypt.ListRecipients(dir)
	for _, p := range o.paths() {
		if _, err := agecrypt.LoadIdentities(p); err != nil {
			continue
		}
		mine, err := agecrypt.IdentityRecipient(p)
		if err != nil {
			return true // not an X25519 key: decryption decides
		}
		if kerr != nil {
			continue
		}
		for _, k := range keys {
			if agecrypt.CanonLine(k) == mine {
				return true
			}
		}
	}
	return false
}

// ReadFile returns the plaintext of p: ciphertext is decrypted with any of the
// identities, anything else is returned as it is.
func (o ageOpener) ReadFile(p string) ([]byte, error) {
	b, err := os.ReadFile(p)
	if err != nil || !agecrypt.IsCiphertext(b) {
		return b, err
	}
	var ids []age.Identity
	var lastErr error
	for _, ip := range o.paths() {
		got, err := agecrypt.LoadIdentities(ip)
		if err != nil {
			lastErr = err
			continue
		}
		ids = append(ids, got...)
	}
	if len(ids) == 0 {
		return nil, lastErr
	}
	return agecrypt.Decrypt(b, ids)
}

// opener is the related opener for this host: relatedOpener, and when that is
// the stock ageOpener, with the identity of each of this host's own encrypted
// sync repos added to the default one.
func (rc *relCtx) opener() related.Opener {
	if a, ok := relatedOpener.(ageOpener); ok && len(a.Extra) == 0 {
		for _, name := range rc.set.names() {
			repo := gitwt.New(rc.set.Cfgs[name].SyncDir)
			if repo.IsRepo() && agecrypt.Configured(repo) {
				if p := agecrypt.IdentityPath(repo); p != "" {
					a.Extra = append(a.Extra, p)
				}
			}
		}
		return a
	}
	return relatedOpener
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
