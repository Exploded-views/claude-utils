package agecrypt

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"filippo.io/age"
	"github.com/MarimerLLC/claude-utils/internal/proc"
)

// MergeContext holds what a merge driver needs when git hands it ciphertext.
type MergeContext struct {
	ids  []age.Identity
	rcpt *Recipients
}

// OpenMergeContext loads the identity and recipients for the clone at dir (a
// driver's working directory is the work-tree root).
func OpenMergeContext(dir string) (*MergeContext, error) {
	f, err := NewFilter(dir)
	if err != nil {
		return nil, err
	}
	ids, err := LoadIdentities(f.IdentityPath)
	if err != nil {
		return nil, err
	}
	r, err := f.recipients()
	if err != nil {
		return nil, err
	}
	return &MergeContext{ids: ids, rcpt: r}, nil
}

// MergeFunc merges three plaintexts; conflict reports unresolved conflicts.
type MergeFunc func(base, ours, theirs []byte) (merged []byte, conflict bool, err error)

// Merge runs fn over the decrypted versions. If ours or theirs is ciphertext,
// the merged result (conflict markers included) is encrypted again, so only
// ciphertext is ever written to %A. ctx may be nil when nothing is encrypted.
func (c *MergeContext) Merge(fn MergeFunc, base, ours, theirs []byte) ([]byte, bool, error) {
	enc := IsCiphertext(ours) || IsCiphertext(theirs) || IsCiphertext(base)
	if enc && c == nil {
		return nil, false, errors.New("encrypted input but no identity available")
	}
	dec := func(b []byte) ([]byte, error) {
		if !IsCiphertext(b) {
			return b, nil
		}
		return Decrypt(b, c.ids)
	}
	var pts [3][]byte
	for i, b := range [][]byte{base, ours, theirs} {
		p, err := dec(b)
		if err != nil {
			return nil, false, fmt.Errorf("cannot decrypt merge input: %w", err)
		}
		pts[i] = p
	}
	merged, conflict, err := fn(pts[0], pts[1], pts[2])
	if err != nil {
		return nil, false, err
	}
	if !enc {
		return merged, conflict, nil
	}
	ct, err := Encrypt(merged, c.rcpt)
	return ct, conflict, err
}

// TextMerge is a 3-way line merge through `git merge-file`, with conflict
// markers of the given size (0 = git's default).
func TextMerge(markerSize int) MergeFunc { return textMerge(markerSize, false) }

// UnionMerge is the line merge that keeps both sides' lines and never
// conflicts (`git merge-file --union`): for append-only files such as
// archive/INDEX.md.
func UnionMerge() MergeFunc { return textMerge(0, true) }

func textMerge(markerSize int, union bool) MergeFunc {
	return func(base, ours, theirs []byte) ([]byte, bool, error) {
		dir, err := os.MkdirTemp("", "memtext-")
		if err != nil {
			return nil, false, err
		}
		defer os.RemoveAll(dir)
		paths := [3]string{dir + "/ours", dir + "/base", dir + "/theirs"}
		for i, b := range [][]byte{ours, base, theirs} {
			if err := os.WriteFile(paths[i], b, 0o600); err != nil {
				return nil, false, err
			}
		}
		args := []string{"merge-file", "-p", "-L", "ours", "-L", "base", "-L", "theirs"}
		if union {
			args = append(args, "--union")
		}
		if markerSize > 0 {
			args = append(args, "--marker-size", strconv.Itoa(markerSize))
		}
		cmd := exec.Command("git", append(args, paths[:]...)...)
		proc.HideWindow(cmd)
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		err = cmd.Run()
		var ee *exec.ExitError
		switch {
		case err == nil:
			return out.Bytes(), false, nil
		case errors.As(err, &ee) && ee.ExitCode() > 0 && ee.ExitCode() < 128:
			return out.Bytes(), true, nil
		}
		return nil, false, fmt.Errorf("git merge-file: %w: %s", err, bytes.TrimSpace(stderr.Bytes()))
	}
}
