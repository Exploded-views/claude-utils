package agecrypt

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fixture struct {
	f       *Filter
	dir     string
	rcpt    string
	idPath  string
	indexed map[string][]byte
}

func newFilterFixture(t *testing.T) *fixture {
	t.Helper()
	idPath, rcpt := newIdentity(t, "id")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, RecipientsFile), []byte(rcpt+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fx := &fixture{dir: dir, rcpt: rcpt, idPath: idPath, indexed: map[string][]byte{}}
	fx.f = &Filter{
		Dir:          dir,
		CacheDir:     filepath.Join(t.TempDir(), "cache"),
		IdentityPath: idPath,
		Indexed: func(p string) ([]byte, error) {
			if b, ok := fx.indexed[p]; ok {
				return b, nil
			}
			return nil, os.ErrNotExist
		},
	}
	return fx
}

func TestFilterClean_Deterministic(t *testing.T) {
	fx := newFilterFixture(t)
	a, err := fx.f.Clean("MEMORY.md", []byte("- one\n"))
	if err != nil || !IsCiphertext(a) {
		t.Fatalf("clean: %v", err)
	}
	b, _ := fx.f.Clean("other.md", []byte("- one\n"))
	if !bytes.Equal(a, b) {
		t.Error("same plaintext must give the same blob")
	}
	c, _ := fx.f.Clean("MEMORY.md", []byte("- two\n"))
	if bytes.Equal(a, c) {
		t.Error("different plaintext must differ")
	}
}

func TestFilterSmudgeThenCleanReturnsCommittedBlob(t *testing.T) {
	fx := newFilterFixture(t)
	rs, _ := os.ReadFile(filepath.Join(fx.dir, RecipientsFile))
	r, _ := ParseRecipients(rs)
	ct, _ := Encrypt([]byte("remote note\n"), r) // made elsewhere: random nonce
	pt := fx.f.Smudge("n.md", ct)
	if string(pt) != "remote note\n" {
		t.Fatalf("smudge = %q", pt)
	}
	fx.indexed["n.md"] = ct
	back, err := fx.f.Clean("n.md", pt)
	if err != nil || !bytes.Equal(back, ct) {
		t.Fatalf("clean after smudge must return the committed blob (err %v)", err)
	}
}

func TestFilterClean_CacheMissStillMatchesIndexedBlob(t *testing.T) {
	fx := newFilterFixture(t)
	rs, _ := os.ReadFile(filepath.Join(fx.dir, RecipientsFile))
	r, _ := ParseRecipients(rs)
	ct, _ := Encrypt([]byte("same\n"), r)
	fx.indexed["n.md"] = ct // never smudged here, cache empty
	back, err := fx.f.Clean("n.md", []byte("same\n"))
	if err != nil || !bytes.Equal(back, ct) {
		t.Fatalf("unchanged plaintext must reuse the indexed blob (err %v)", err)
	}
}

func TestFilterClean_CiphertextPassThroughOnlyIfIdentical(t *testing.T) {
	fx := newFilterFixture(t)
	rs, _ := os.ReadFile(filepath.Join(fx.dir, RecipientsFile))
	r, _ := ParseRecipients(rs)
	ct, _ := Encrypt([]byte("secret\n"), r)
	fx.indexed["n.md"] = ct
	if out, err := fx.f.Clean("n.md", ct); err != nil || !bytes.Equal(out, ct) {
		t.Fatalf("identical ciphertext must pass: %v", err)
	}
	// Keyless-append hazard: ciphertext with a plaintext line appended.
	if _, err := fx.f.Clean("n.md", append(append([]byte{}, ct...), []byte("\n- appended\n")...)); err == nil {
		t.Error("modified ciphertext must be refused")
	}
	if _, err := fx.f.Clean("new.md", ct); err == nil {
		t.Error("ciphertext with no indexed counterpart must be refused")
	}
}

func TestFilterSmudge_KeylessAndPlaintextPassThrough(t *testing.T) {
	fx := newFilterFixture(t)
	other, orcpt := newIdentity(t, "other")
	_ = other
	r, _ := ParseRecipients([]byte(orcpt))
	ct, _ := Encrypt([]byte("not for us"), r)
	if out := fx.f.Smudge("n.md", ct); !bytes.Equal(out, ct) {
		t.Error("undecryptable blob must pass through unchanged")
	}
	if out := fx.f.Smudge("n.md", []byte("plain\n")); string(out) != "plain\n" {
		t.Error("plaintext blob must pass through")
	}
	fx.f.IdentityPath = filepath.Join(t.TempDir(), "gone")
	if out := fx.f.Smudge("n.md", ct); !bytes.Equal(out, ct) {
		t.Error("missing identity must pass ciphertext through")
	}
}

func TestFilterClean_NoRecipientsRefuses(t *testing.T) {
	fx := newFilterFixture(t)
	os.Remove(filepath.Join(fx.dir, RecipientsFile))
	if _, err := fx.f.Clean("n.md", []byte("x")); err == nil {
		t.Error("must refuse to store plaintext when there are no recipients")
	}
}

func TestFilterTextconv(t *testing.T) {
	fx := newFilterFixture(t)
	ct, _ := fx.f.Clean("n.md", []byte("readable\n"))
	if string(fx.f.Textconv(ct)) != "readable\n" {
		t.Error("textconv should decrypt")
	}
	if string(fx.f.Textconv([]byte("plain"))) != "plain" {
		t.Error("textconv passes plaintext")
	}
}

// pkt-line helpers for driving Serve like git does.
func pkt(s string) string { return strings.Replace("0000"+s, "0000", hex4(len(s)+4), 1) }
func hex4(n int) string {
	const d = "0123456789abcdef"
	return string([]byte{d[n>>12&15], d[n>>8&15], d[n>>4&15], d[n&15]})
}

const flush = "0000"

func TestServe_Protocol(t *testing.T) {
	fx := newFilterFixture(t)
	var in strings.Builder
	in.WriteString(pkt("git-filter-client\n") + pkt("version=2\n") + flush)
	in.WriteString(pkt("capability=clean\n") + pkt("capability=smudge\n") + pkt("capability=delay\n") + flush)
	in.WriteString(pkt("command=clean\n") + pkt("pathname=a.md\n") + flush + pkt("hello\n") + flush)
	var out bytes.Buffer
	if err := fx.f.Serve(strings.NewReader(in.String()), &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	want := pkt("git-filter-server\n") + pkt("version=2\n") + flush + pkt("capability=clean\n") + pkt("capability=smudge\n") + flush + pkt("status=success\n") + flush
	if !strings.HasPrefix(got, want) {
		t.Fatalf("handshake/status mismatch:\n%q\nwant prefix\n%q", got, want)
	}
	// content packets then flush, then the empty final status list.
	rest := got[len(want):]
	if !strings.HasSuffix(rest, flush+flush) {
		t.Fatalf("response must end with content flush and status flush: %q", rest[len(rest)-8:])
	}
}

func TestServe_ErrorStatusAndLargeContent(t *testing.T) {
	fx := newFilterFixture(t)
	big := strings.Repeat("0123456789abcdef", 20000) // > one pkt-line
	var in strings.Builder
	in.WriteString(pkt("git-filter-client\n") + pkt("version=2\n") + flush + pkt("capability=clean\n") + pkt("capability=smudge\n") + flush)
	// ciphertext-looking content with no indexed blob => error
	in.WriteString(pkt("command=clean\n") + pkt("pathname=x.md\n") + flush + pkt(string(foreignCT(t))) + flush)
	// big plaintext: success, and smudge of the result must round-trip
	in.WriteString(pkt("command=clean\n") + pkt("pathname=big.md\n") + flush)
	for i := 0; i < len(big); i += 60000 {
		end := i + 60000
		if end > len(big) {
			end = len(big)
		}
		in.WriteString(pkt(big[i:end]))
	}
	in.WriteString(flush)
	var out bytes.Buffer
	if err := fx.f.Serve(strings.NewReader(in.String()), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), pkt("status=error\n")) {
		t.Error("refused clean must report status=error")
	}
	if !strings.Contains(out.String(), pkt("status=success\n")) {
		t.Error("the next request must still be served")
	}
	_ = io.EOF
}

// foreignCT is real ciphertext for a key that is not in the fixture.
func foreignCT(t *testing.T) []byte {
	t.Helper()
	_, r := newIdentity(t, "foreign")
	ct, err := Encrypt([]byte("not ours"), mustRecipients(t, r))
	if err != nil {
		t.Fatal(err)
	}
	return ct
}
