package hosts

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

var (
	labelRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	fpRe    = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)
	slugRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)
)

// ValidLabel reports whether s is a host label.
func ValidLabel(s string) bool { return labelRe.MatchString(s) }

// ValidFingerprint reports whether s is a SHA256:... key fingerprint as GitHub
// shows it. Anything else, key text in particular, is refused.
func ValidFingerprint(s string) bool { return fpRe.MatchString(s) }

// ValidSlug reports whether s can be a GitHub App slug.
func ValidSlug(s string) bool { return slugRe.MatchString(s) }

// ReadRegistry reads hosts/<label>/ of the registry work-tree: the published
// expiry and check-key record of each host. A missing hosts directory is an
// empty registry; directories whose name is not a label are ignored; an
// unreadable file is reported on the host, never fatal.
func ReadRegistry(dir string) (map[string]RegHost, error) {
	out := map[string]RegHost{}
	ents, err := os.ReadDir(filepath.Join(dir, "hosts"))
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		if !e.IsDir() || !labelRe.MatchString(e.Name()) {
			continue
		}
		h := RegHost{Label: e.Name()}
		hd := filepath.Join(dir, "hosts", e.Name())
		if b, err := os.ReadFile(filepath.Join(hd, "expiry")); err == nil {
			if t, perr := time.Parse("2006-01-02", strings.TrimSpace(string(b))); perr == nil {
				h.Expiry = t
			} else {
				h.Problem = "expiry file is not a YYYY-MM-DD date"
			}
		}
		if b, err := os.ReadFile(filepath.Join(hd, "check-key")); err == nil {
			var ck CheckKey
			if json.Unmarshal(b, &ck) == nil && fpRe.MatchString(ck.Fingerprint) && slugRe.MatchString(ck.App) {
				h.CheckKey = &ck
			} else {
				h.Problem = "check-key file is not a SHA256 fingerprint and app slug"
			}
		}
		out[e.Name()] = h
	}
	return out, nil
}

// commitFile stages rel (a path under dir, removed or not) and commits just
// that path locally. It never pushes: the registry is a normal sync dir whose
// own daemon does. It reports whether a commit was made.
func commitFile(dir, rel, msg string) (bool, error) {
	g := gitwt.New(dir)
	if !g.IsRepo() {
		return false, fmt.Errorf("registry %s is not a git work-tree", dir)
	}
	if _, err := g.Run("add", "-A", "--", gitwt.Literal(rel)); err != nil {
		return false, err
	}
	st, err := g.Run("status", "--porcelain", "--", gitwt.Literal(rel))
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(st) == "" {
		return false, nil
	}
	if _, err := g.Run("commit", "-q", "-m", msg, "--", gitwt.Literal(rel)); err != nil {
		return false, err
	}
	return true, nil
}

func hostFile(dir, label, name string) (string, string, error) {
	if !labelRe.MatchString(label) {
		return "", "", fmt.Errorf("label %q: use 1-32 characters of a-z, 0-9 and '-', starting with a letter or digit", label)
	}
	rel := filepath.ToSlash(filepath.Join("hosts", label, name))
	return rel, filepath.Join(dir, filepath.FromSlash(rel)), nil
}

// WriteCheckKey records a host's check-App key fingerprint and App slug in
// hosts/<label>/check-key and commits that file locally. Only a SHA256
// fingerprint is accepted, never key text. It reports whether anything changed.
func WriteCheckKey(dir, label string, ck CheckKey) (bool, error) {
	if !fpRe.MatchString(ck.Fingerprint) {
		return false, errors.New("the fingerprint must look like SHA256:<43 base64 characters>, as GitHub shows it; never the key itself")
	}
	if !slugRe.MatchString(ck.App) {
		return false, errors.New("the app slug must be letters, digits and '-'")
	}
	rel, path, err := hostFile(dir, label, "check-key")
	if err != nil {
		return false, err
	}
	b, _ := json.MarshalIndent(ck, "", "  ")
	b = append(b, '\n')
	if old, err := os.ReadFile(path); err == nil && string(old) == string(b) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return false, err
	}
	return commitFile(dir, rel, "check key record for "+label)
}

// ClearCheckKey removes hosts/<label>/check-key (committed locally).
func ClearCheckKey(dir, label string) error {
	rel, path, err := hostFile(dir, label, "check-key")
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	_, err = commitFile(dir, rel, "clear check key record for "+label)
	return err
}

// RemoveHost removes hosts/<label>/ from the registry work-tree and commits
// the removal locally. It reports whether the entry existed.
func RemoveHost(dir, label string) (bool, error) {
	rel, _, err := hostFile(dir, label, "x")
	if err != nil {
		return false, err
	}
	rel = filepath.ToSlash(filepath.Dir(rel))
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if _, err := os.Stat(full); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if err := os.RemoveAll(full); err != nil {
		return false, err
	}
	if _, err := commitFile(dir, rel, "retire host "+label); err != nil {
		return true, err
	}
	return true, nil
}
