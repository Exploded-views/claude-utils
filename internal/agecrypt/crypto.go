// Package agecrypt encrypts the sync repo at rest with age. It provides the
// pieces git needs to do it transparently: a clean/smudge filter speaking git's
// long-running process protocol, decrypting merge-driver helpers, the config
// that wires them in, a pre-push guard and recipient management.
package agecrypt

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"filippo.io/age"
	"filippo.io/age/agessh"
)

// Header is the first line of every age file (binary format).
const Header = "age-encryption.org/v1"

// IsCiphertext reports whether b is an age file.
func IsCiphertext(b []byte) bool { return bytes.HasPrefix(b, []byte(Header)) }

// Recipients is a parsed .age-recipients file.
type Recipients struct {
	list  []age.Recipient
	canon []string
}

// Len is the number of recipients.
func (r *Recipients) Len() int { return len(r.list) }

// ParseRecipients parses a recipients file: one age1... or ssh-... public key
// per line; blank lines and #-comments are ignored.
func ParseRecipients(data []byte) (*Recipients, error) {
	r := &Recipients{}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		switch {
		case strings.HasPrefix(line, "ssh-"):
			rc, err := agessh.ParseRecipient(line)
			if err != nil {
				return nil, fmt.Errorf("recipients line %d: %w", i+1, err)
			}
			r.list = append(r.list, rc)
			f := strings.Fields(line)
			if len(f) < 2 {
				return nil, fmt.Errorf("recipients line %d: malformed ssh key", i+1)
			}
			r.canon = append(r.canon, f[0]+" "+f[1]) // drop the comment
		default:
			rcs, err := age.ParseRecipients(strings.NewReader(line))
			if err != nil {
				return nil, fmt.Errorf("recipients line %d: %w", i+1, err)
			}
			r.list = append(r.list, rcs...)
			r.canon = append(r.canon, line)
		}
	}
	return r, nil
}

// Fingerprint identifies the set of recipients, ignoring order and comments.
func (r *Recipients) Fingerprint() string {
	c := append([]string(nil), r.canon...)
	sort.Strings(c)
	sum := sha256.Sum256([]byte(strings.Join(c, "\n")))
	return hex.EncodeToString(sum[:8])
}

// Encrypt encrypts pt to r. It refuses an empty recipient set so that a missing
// or empty file can never silently produce unreadable (or plaintext) data.
func Encrypt(pt []byte, r *Recipients) ([]byte, error) {
	if r == nil || r.Len() == 0 {
		return nil, errors.New("no recipients: .age-recipients is missing or empty")
	}
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, r.list...)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(pt); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Decrypt decrypts ct with ids.
func Decrypt(ct []byte, ids []age.Identity) ([]byte, error) {
	if len(ids) == 0 {
		return nil, errors.New("no identity")
	}
	rd, err := age.Decrypt(bytes.NewReader(ct), ids...)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(rd)
}

// LoadIdentities reads an age identity file (AGE-SECRET-KEY lines) or an
// unencrypted OpenSSH private key.
func LoadIdentities(path string) ([]age.Identity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("identity: %w", err)
	}
	if bytes.Contains(data, []byte("-----BEGIN")) {
		id, err := agessh.ParseIdentity(data)
		if err != nil {
			return nil, fmt.Errorf("identity %s: %w (a passphrase-protected SSH key cannot be used by the daemon)", filepath.Base(path), err)
		}
		return []age.Identity{id}, nil
	}
	ids, err := age.ParseIdentities(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("identity %s: %w", filepath.Base(path), err)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("identity %s: no keys found", filepath.Base(path))
	}
	return ids, nil
}

// GenerateIdentity writes a new X25519 identity to path (mode 0600, never
// overwriting) and returns its public recipient. The secret is not returned.
func GenerateIdentity(path string) (string, error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("identity: %w", err)
	}
	_, werr := fmt.Fprintf(f, "# claude-memsync age identity; keep private, back up offline\n# public key: %s\n%s\n", id.Recipient(), id)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(path)
		return "", werr
	}
	return id.Recipient().String(), nil
}

// IdentityRecipient returns the public recipient of an X25519 identity file.
func IdentityRecipient(path string) (string, error) {
	ids, err := LoadIdentities(path)
	if err != nil {
		return "", err
	}
	for _, id := range ids {
		if x, ok := id.(*age.X25519Identity); ok {
			return x.Recipient().String(), nil
		}
	}
	return "", errors.New("identity is not an age X25519 key; pass its public key with --recipient")
}
