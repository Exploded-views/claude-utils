package agecrypt

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// RecipientsFile is the committed, plaintext list of public keys.
const RecipientsFile = ".age-recipients"

// ReencryptEnv, when set to "1" in the filter's environment, makes clean
// encrypt afresh instead of reusing an indexed blob that decrypts to the same
// plaintext. `recipients add|remove` sets it for the renormalize.
const ReencryptEnv = "MEMAGE_REENCRYPT"

// Filter is git's clean/smudge/textconv filter for age. It is deterministic:
// the ciphertext for a given plaintext and recipient set is cached, and smudge
// seeds the cache, so re-cleaning a checked-out file yields the committed blob
// and the work-tree stays clean after a checkout or rebase.
type Filter struct {
	Dir          string                            // work-tree root (holds .age-recipients)
	CacheDir     string                            // ciphertext cache, under the git dir
	IdentityPath string                            // age identity; may be missing (keyless)
	Indexed      func(path string) ([]byte, error) // the blob currently in the index
}

// NewFilter builds a Filter for the git work-tree containing dir, reading the
// identity path from `git config memage.identity`.
func NewFilter(dir string) (*Filter, error) {
	gitDir, err := gitOut(dir, "rev-parse", "--git-common-dir")
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(dir, gitDir)
	}
	f := &Filter{Dir: dir, CacheDir: filepath.Join(gitDir, "memage-cache")}
	f.IdentityPath, _ = gitOut(dir, "config", "--get", IdentityConfigKey)
	f.Indexed = func(p string) ([]byte, error) {
		cmd := exec.Command("git", "-C", dir, "cat-file", "blob", ":0:"+p)
		return cmd.Output()
	}
	return f, nil
}

func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func (f *Filter) recipients() (*Recipients, error) {
	data, err := os.ReadFile(filepath.Join(f.Dir, RecipientsFile))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", RecipientsFile, err)
	}
	return ParseRecipients(data)
}

func cacheName(pt []byte, fp string) string {
	sum := sha256.Sum256(pt)
	return hex.EncodeToString(sum[:]) + "-" + fp
}

func (f *Filter) cacheGet(name string) ([]byte, bool) {
	b, err := os.ReadFile(filepath.Join(f.CacheDir, name))
	if err != nil || !IsCiphertext(b) {
		return nil, false
	}
	return b, true
}

func (f *Filter) cachePut(name string, ct []byte) {
	if os.MkdirAll(f.CacheDir, 0o700) != nil {
		return
	}
	tmp, err := os.CreateTemp(f.CacheDir, ".tmp-")
	if err != nil {
		return
	}
	_, werr := tmp.Write(ct)
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Rename(tmp.Name(), filepath.Join(f.CacheDir, name)) != nil {
		os.Remove(tmp.Name())
	}
}

// Clean turns work-tree content into the blob to store.
func (f *Filter) Clean(path string, in []byte) ([]byte, error) {
	if IsCiphertext(in) {
		// A keyless clone holds ciphertext in its work-tree. It may pass back
		// unchanged, but never edited: an append would store a blob that no
		// longer decrypts and carries plaintext.
		idx, err := f.Indexed(path)
		if err == nil && bytes.Equal(idx, in) {
			return in, nil
		}
		return nil, errors.New("refusing to store modified ciphertext (this host cannot decrypt it; add its key with `recipients add`)")
	}
	rcpt, err := f.recipients()
	if err != nil {
		return nil, fmt.Errorf("refusing to store plaintext: %w", err)
	}
	if rcpt.Len() == 0 {
		return nil, errors.New("refusing to store plaintext: no recipients in " + RecipientsFile)
	}
	name := cacheName(in, rcpt.Fingerprint())
	if ct, ok := f.cacheGet(name); ok {
		return ct, nil
	}
	if os.Getenv(ReencryptEnv) != "1" {
		// Cache lost: an unchanged file must still give the committed blob.
		if idx, err := f.Indexed(path); err == nil && IsCiphertext(idx) {
			if ids, err := LoadIdentities(f.IdentityPath); err == nil {
				if pt, err := Decrypt(idx, ids); err == nil && bytes.Equal(pt, in) {
					f.cachePut(name, idx)
					return idx, nil
				}
			}
		}
	}
	ct, err := Encrypt(in, rcpt)
	if err != nil {
		return nil, err
	}
	f.cachePut(name, ct)
	return ct, nil
}

// Smudge turns a stored blob into work-tree content. Anything it cannot
// decrypt passes through unchanged, so a keyless clone still checks out.
func (f *Filter) Smudge(path string, in []byte) []byte {
	if !IsCiphertext(in) {
		return in
	}
	ids, err := LoadIdentities(f.IdentityPath)
	if err != nil {
		return in
	}
	pt, err := Decrypt(in, ids)
	if err != nil {
		return in
	}
	if rcpt, err := f.recipients(); err == nil && rcpt.Len() > 0 {
		f.cachePut(cacheName(pt, rcpt.Fingerprint()), in)
	}
	return pt
}

// Textconv shows a blob's plaintext for diff and log.
func (f *Filter) Textconv(in []byte) []byte {
	if !IsCiphertext(in) {
		return in
	}
	ids, err := LoadIdentities(f.IdentityPath)
	if err != nil {
		return in
	}
	if pt, err := Decrypt(in, ids); err == nil {
		return pt
	}
	return in
}

// ClearCache drops every cached ciphertext.
func (f *Filter) ClearCache() error { return os.RemoveAll(f.CacheDir) }

// maxPkt is the largest payload of one pkt-line.
const maxPkt = 65516

func readPkt(r io.Reader) (data []byte, isFlush bool, err error) {
	var hdr [4]byte
	if _, err = io.ReadFull(r, hdr[:]); err != nil {
		return nil, false, err
	}
	var n int
	for _, c := range hdr {
		var v int
		switch {
		case c >= '0' && c <= '9':
			v = int(c - '0')
		case c >= 'a' && c <= 'f':
			v = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v = int(c-'A') + 10
		default:
			return nil, false, fmt.Errorf("bad pkt-line length %q", hdr[:])
		}
		n = n<<4 | v
	}
	if n == 0 {
		return nil, true, nil
	}
	if n < 4 {
		return nil, false, fmt.Errorf("bad pkt-line length %d", n)
	}
	data = make([]byte, n-4)
	if _, err = io.ReadFull(r, data); err != nil {
		return nil, false, err
	}
	return data, false, nil
}

func writePkt(w io.Writer, s string) error {
	_, err := fmt.Fprintf(w, "%04x%s", len(s)+4, s)
	return err
}

func writeFlush(w io.Writer) error { _, err := io.WriteString(w, "0000"); return err }

// readList reads packets up to a flush. EOF before any packet returns io.EOF.
func readList(r io.Reader) ([]string, error) {
	var out []string
	for {
		d, fl, err := readPkt(r)
		if err != nil {
			if err == io.EOF && len(out) > 0 {
				err = io.ErrUnexpectedEOF
			}
			return out, err
		}
		if fl {
			return out, nil
		}
		out = append(out, strings.TrimSuffix(string(d), "\n"))
	}
}

func readContent(r io.Reader) ([]byte, error) {
	var buf bytes.Buffer
	for {
		d, fl, err := readPkt(r)
		if err != nil {
			return nil, err
		}
		if fl {
			return buf.Bytes(), nil
		}
		buf.Write(d)
	}
}

// Serve speaks git's long-running filter protocol (version 2) on r and w until
// git closes the input.
func (f *Filter) Serve(r io.Reader, w io.Writer) error {
	hello, err := readList(r)
	if err != nil {
		return err
	}
	if len(hello) < 2 || hello[0] != "git-filter-client" || !contains(hello, "version=2") {
		return fmt.Errorf("unsupported filter handshake %v", hello)
	}
	if err := writePkt(w, "git-filter-server\n"); err != nil {
		return err
	}
	if err := writePkt(w, "version=2\n"); err != nil {
		return err
	}
	if err := writeFlush(w); err != nil {
		return err
	}
	caps, err := readList(r)
	if err != nil {
		return err
	}
	for _, c := range []string{"clean", "smudge"} {
		if contains(caps, "capability="+c) {
			if err := writePkt(w, "capability="+c+"\n"); err != nil {
				return err
			}
		}
	}
	if err := writeFlush(w); err != nil {
		return err
	}
	for {
		hdr, err := readList(r)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		var cmd, path string
		for _, h := range hdr {
			if v, ok := strings.CutPrefix(h, "command="); ok {
				cmd = v
			} else if v, ok := strings.CutPrefix(h, "pathname="); ok {
				path = v
			}
		}
		in, err := readContent(r)
		if err != nil {
			return err
		}
		var out []byte
		var ferr error
		switch cmd {
		case "clean":
			out, ferr = f.Clean(path, in)
		case "smudge":
			out = f.Smudge(path, in)
		default:
			ferr = fmt.Errorf("unsupported command %q", cmd)
		}
		if ferr != nil {
			fmt.Fprintf(os.Stderr, "memage: %s %s: %v\n", cmd, path, ferr)
			if err := writePkt(w, "status=error\n"); err != nil {
				return err
			}
			if err := writeFlush(w); err != nil {
				return err
			}
			continue
		}
		if err := writePkt(w, "status=success\n"); err != nil {
			return err
		}
		if err := writeFlush(w); err != nil {
			return err
		}
		for len(out) > 0 {
			n := min(len(out), maxPkt)
			if err := writePkt(w, string(out[:n])); err != nil {
				return err
			}
			out = out[n:]
		}
		if err := writeFlush(w); err != nil { // end of content
			return err
		}
		if err := writeFlush(w); err != nil { // empty final status: unchanged
			return err
		}
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
