package agecrypt

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// Encryption schemes. SchemePQ (the default for a new repo) encrypts to hybrid
// ML-KEM-768 + X25519 recipients only; SchemeClassic keeps upstream's X25519
// and ssh recipients. A repo with no scheme recorded is classic.
const (
	SchemeClassic = "classic"
	SchemePQ      = "pq"

	// SchemeConfigKey is the repo-local git config key mirroring the config's
	// encryptionScheme, so the commands below can enforce it without the config.
	SchemeConfigKey = "memage.scheme"

	// HybridStanza is the age stanza type of an ML-KEM-768 + X25519 recipient.
	HybridStanza = "mlkem768x25519"

	hybridRecipientPrefix = "age1pq1"
)

// ValidScheme reports whether s names an encryption scheme.
func ValidScheme(s string) bool { return s == SchemeClassic || s == SchemePQ }

// IsHybridRecipient reports whether a recipients line (or bare key) is a
// hybrid post-quantum public key.
func IsHybridRecipient(line string) bool {
	return strings.HasPrefix(canonLine(line), hybridRecipientPrefix)
}

// SchemeOf is the repo's encryption scheme ("classic" when none is recorded).
func SchemeOf(repo *gitwt.Repo) string {
	out, _ := repo.Run("config", "--get", SchemeConfigKey)
	if strings.TrimSpace(out) == SchemePQ {
		return SchemePQ
	}
	return SchemeClassic
}

// SetScheme records the scheme in the clone's git config.
func SetScheme(repo *gitwt.Repo, scheme string) error {
	if !ValidScheme(scheme) {
		return fmt.Errorf("unknown encryption scheme %q (use pq or classic)", scheme)
	}
	return repo.ConfigSet(SchemeConfigKey, scheme)
}

// ErrNotPQ is returned for a recipient that is not a hybrid key under pq.
var ErrNotPQ = errors.New("this repo is post-quantum (encryptionScheme pq): only hybrid age1pq1... recipients are accepted; classic age1... and ssh keys are refused")

// ErrHybridInClassic is returned for a hybrid key in a classic repo.
var ErrHybridInClassic = errors.New("this repo uses classic encryption (encryptionScheme classic): a hybrid age1pq1... key cannot be mixed with classic recipients; migrate the repo with `recipients rekey-pq`")

// CheckKeysForScheme refuses any recipient line that does not fit the scheme:
// under pq every key must be a valid hybrid key; under classic none may be.
func CheckKeysForScheme(scheme string, keys []string) error {
	for _, k := range keys {
		if !isKeyLine(k) {
			continue
		}
		hybrid := IsHybridRecipient(k)
		switch {
		case scheme == SchemePQ && !hybrid:
			return ErrNotPQ
		case scheme == SchemePQ:
			if _, err := age.ParseHybridRecipient(canonLine(k)); err != nil {
				return fmt.Errorf("not a valid hybrid age public key (key %s)", keyPrint(canonLine(k)))
			}
		case hybrid:
			return ErrHybridInClassic
		}
	}
	return nil
}

// GenerateHybridIdentity writes a new hybrid post-quantum identity to path
// (mode 0600, never overwriting) and returns its public recipient. The secret
// is never returned.
func GenerateHybridIdentity(path string) (string, error) {
	id, err := age.GenerateHybridIdentity()
	if err != nil {
		return "", err
	}
	return writeHybridIdentity(path, id)
}

func writeHybridIdentity(path string, id *age.HybridIdentity) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("identity: %w", err)
	}
	_, werr := fmt.Fprintf(f, "# claude-memsync age identity (post-quantum hybrid); keep private, back up offline\n# public key: %s\n%s\n", id.Recipient(), id)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(path)
		return "", werr
	}
	return id.Recipient().String(), nil
}

// StanzaTypes lists the recipient stanza types in an age file's header.
func StanzaTypes(ct []byte) []string {
	rest, ok := bytes.CutPrefix(ct, []byte(Header+"\n"))
	if !ok {
		return nil
	}
	var out []string
	for len(rest) > 0 {
		var line []byte
		line, rest, _ = bytes.Cut(rest, []byte("\n"))
		if bytes.HasPrefix(line, []byte("--- ")) {
			return out
		}
		if t, ok := bytes.CutPrefix(line, []byte("-> ")); ok {
			f := strings.Fields(string(t))
			if len(f) > 0 {
				out = append(out, f[0])
			}
		}
	}
	return out
}

// IsHybridCiphertext reports whether b is age ciphertext whose recipient
// stanzas are hybrid only: at least one mlkem768x25519 stanza and no classic
// one (X25519, ssh, scrypt). age's random grease stanzas are ignored.
func IsHybridCiphertext(b []byte) bool {
	if !IsCiphertext(b) {
		return false
	}
	hybrid := false
	for _, t := range StanzaTypes(b) {
		switch t {
		case HybridStanza:
			hybrid = true
		case "X25519", "ssh-ed25519", "ssh-rsa", "scrypt":
			return false
		}
	}
	return hybrid
}
