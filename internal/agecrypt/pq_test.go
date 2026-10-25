package agecrypt

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"filippo.io/age/agessh"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/testenv"
)

func newPQIdentity(t *testing.T, name string) (string, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	rcpt, err := GenerateHybridIdentity(p)
	if err != nil {
		t.Fatal(err)
	}
	return p, rcpt
}

// newHostPQ is newHost with the pq scheme and a hybrid identity.
func newHostPQ(t *testing.T, remote string, first bool, extra ...string) *host {
	t.Helper()
	ms, mm := testenv.Binaries(t)
	idPath, rcpt := newPQIdentity(t, "id")
	dir := filepath.Join(t.TempDir(), "sync")
	repo := gitwt.New(dir)
	if first {
		if err := repo.Init("main"); err != nil {
			t.Fatal(err)
		}
		if err := repo.SetRemote(remote); err != nil {
			t.Fatal(err)
		}
	} else if err := repo.CloneNoCheckout(remote); err != nil {
		t.Fatal(err)
	}
	if err := Configure(repo, Opts{MemsyncExe: ms, MemmergeExe: mm, IdentityPath: idPath, Scheme: SchemePQ}); err != nil {
		t.Fatal(err)
	}
	if first {
		if err := WriteRecipients(dir, append([]string{rcpt}, extra...)); err != nil {
			t.Fatal(err)
		}
		if err := WriteAttributes(dir); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("config.json\n"), 0o600)
		if err := PinCurrent(repo); err != nil {
			t.Fatal(err)
		}
	} else if repo.HasHead() {
		if _, err := repo.Run("checkout", "-q", "main"); err != nil {
			t.Fatal(err)
		}
		if err := PinCurrent(repo); err != nil {
			t.Fatal(err)
		}
	}
	return &host{repo: repo, idPath: idPath, rcpt: rcpt}
}

func TestPQ_KeysAndIdentityShapes(t *testing.T) {
	p, rcpt := newPQIdentity(t, "id")
	if !strings.HasPrefix(rcpt, "age1pq1") || len(rcpt) < 1500 {
		t.Fatalf("recipient shape: %d chars", len(rcpt))
	}
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("identity mode: %v %v", fi, err)
	}
	got, err := IdentityRecipient(p)
	if err != nil || got != rcpt {
		t.Fatalf("IdentityRecipient: %v", err)
	}
	if _, err := GenerateHybridIdentity(p); err == nil {
		t.Error("must not overwrite an identity")
	}
	if ids, err := LoadIdentities(p); err != nil || len(ids) != 1 {
		t.Fatalf("load: %v", err)
	}
}

func TestPQ_EncryptDecryptStanzaAndMixing(t *testing.T) {
	p, rcpt := newPQIdentity(t, "id")
	r := mustRecipients(t, rcpt+" # host:vm-1")
	ct, err := Encrypt([]byte("hello"), r)
	if err != nil {
		t.Fatal(err)
	}
	if !IsCiphertext(ct) || !IsHybridCiphertext(ct) {
		t.Fatal("hybrid ciphertext must be recognised")
	}
	if types := StanzaTypes(ct); len(types) == 0 || types[0] != HybridStanza && !contains(types, HybridStanza) {
		t.Fatalf("stanza types: %v", types)
	}
	ids, _ := LoadIdentities(p)
	if pt, err := Decrypt(ct, ids); err != nil || string(pt) != "hello" {
		t.Fatalf("decrypt: %q %v", pt, err)
	}
	// classic ciphertext is ciphertext, but not hybrid
	_, classic := newIdentity(t, "c")
	cct, err := Encrypt([]byte("x"), mustRecipients(t, classic))
	if err != nil || !IsCiphertext(cct) || IsHybridCiphertext(cct) {
		t.Fatalf("classic must not count as hybrid: %v", err)
	}
	// age refuses a mixed recipient set
	if _, err := Encrypt([]byte("x"), mustRecipients(t, rcpt, classic)); err == nil {
		t.Error("mixing hybrid and classic recipients must fail")
	}
	// plaintext starting with the header is not hybrid ciphertext
	if IsHybridCiphertext([]byte(Header + "\nhello")) {
		t.Error("bare header")
	}
}

func TestPQ_CheckKeysForScheme(t *testing.T) {
	_, pq := newPQIdentity(t, "p")
	_, cl := newIdentity(t, "c")
	ssh := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHcbV3DnP0R7ZqzuZXlzJ4W0eKgQh8WZ1xX3nYF8mO6c"
	if err := CheckKeysForScheme(SchemePQ, []string{pq + " # host:a", pq + " # recovery"}); err != nil {
		t.Errorf("pq keys under pq: %v", err)
	}
	for name, k := range map[string]string{"classic": cl, "ssh": ssh, "truncated": pq[:300]} {
		if err := CheckKeysForScheme(SchemePQ, []string{k}); err == nil {
			t.Errorf("%s must be refused under pq", name)
		}
	}
	if err := CheckKeysForScheme(SchemeClassic, []string{cl, ssh}); err != nil {
		t.Errorf("classic unchanged: %v", err)
	}
	if err := CheckKeysForScheme(SchemeClassic, []string{pq}); err == nil {
		t.Error("hybrid refused in a classic repo")
	}
	if _, err := agessh.ParseRecipient(ssh); err == nil {
		_ = err
	}
}

func TestPQ_RecipientsLinesLabelsAndFingerprints(t *testing.T) {
	_, a := newPQIdentity(t, "a")
	_, b := newPQIdentity(t, "b")
	r, err := ParseRecipients([]byte("# c\n" + a + " # host:vm-1\n" + b + " # recovery\n"))
	if err != nil || r.Len() != 2 {
		t.Fatalf("parse: %v", err)
	}
	if LabelOf(a+" # host:vm-1") != "vm-1" || LabelOf(b+" # recovery") != "" {
		t.Error("labels")
	}
	if canonLine(a+" # host:vm-1") != a {
		t.Error("canonLine must return the long key")
	}
	r2, _ := ParseRecipients([]byte(b + " # recovery\n" + a + " # host:vm-1\n"))
	if r.Fingerprint() != r2.Fingerprint() {
		t.Error("fingerprint ignores order")
	}
	r3, _ := ParseRecipients([]byte(a + " # host:vm-2\n" + b + "\n"))
	if r.Fingerprint() == r3.Fingerprint() {
		t.Error("fingerprint covers labels")
	}
	if len(keyPrint(a)) != 12 || len(JoinFingerprint(a)) != 32 {
		t.Error("short fingerprints")
	}
}

func TestPQ_JoinRequestWithHybridKey(t *testing.T) {
	_, rcpt := newPQIdentity(t, "j")
	j := Join{Label: "host-1", Recipient: rcpt, Fingerprint: JoinFingerprint(rcpt), Requested: joinNow}
	text := j.Format()
	if len(text) <= 1024 || len(text) > maxJoinBytes {
		t.Fatalf("a hybrid request is %d bytes; the cap must fit it but stay bounded", len(text))
	}
	got, err := ParseJoin([]byte(text))
	if err != nil || got.Recipient != rcpt {
		t.Fatalf("parse: %v", err)
	}
	// still strict
	for name, c := range map[string]string{
		"extra line":  text + "x: y\n",
		"truncated":   strings.Replace(text, rcpt, rcpt[:len(rcpt)-4], 1),
		"corrupt key": strings.Replace(text, rcpt, rcpt[:len(rcpt)-1]+"q", 1),
		"huge":        text + strings.Repeat("x", maxJoinBytes),
		"two keys":    strings.Replace(text, rcpt, rcpt+" "+rcpt, 1),
	} {
		if _, err := ParseJoin([]byte(c)); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
	if !ValidJoinBlob("joins/host-1.txt", []byte(text)) {
		t.Error("valid blob")
	}
}

func TestPQ_GitRoundTripFilterAndDrivers(t *testing.T) {
	gitEnv(t)
	remote := bareRemote(t)
	a := newHostPQ(t, remote, true)
	if SchemeOf(a.repo) != SchemePQ {
		t.Fatal("scheme must be recorded")
	}
	a.write(t, "MEMORY.md", "- [base](base.md) hook\n")
	a.write(t, "notes/shared.md", "line1\nline2\nline3\n")
	a.commit(t, "memsync: 2 files")
	for _, p := range []string{"MEMORY.md", "notes/shared.md"} {
		if !IsHybridCiphertext([]byte(a.blob(t, "HEAD", p))) {
			t.Fatalf("%s must be hybrid ciphertext in the repo", p)
		}
	}
	if a.read(t, "notes/shared.md") != "line1\nline2\nline3\n" || a.dirty(t) != "" {
		t.Fatal("work-tree plaintext and clean")
	}
	if err := PushGuard(a.repo, "main"); err != nil {
		t.Fatalf("guard must accept hybrid ciphertext: %v", err)
	}
	a.repo.Run("push", "origin", "main")

	b := newHostPQ(t, remote, false)
	if err := CheckKeyed(b.repo); err == nil {
		t.Fatal("b is keyless")
	}
	if err := AddRecipient(a.repo, b.rcpt); err != nil {
		t.Fatal(err)
	}
	a.repo.Run("push", "origin", "main")
	b.repo.Run("pull", "-q", "--rebase", "origin", "main")
	if err := AcceptRecipients(b.repo); err != nil {
		t.Fatal(err)
	}
	if err := CheckKeyed(b.repo); err != nil || b.read(t, "MEMORY.md") != "- [base](base.md) hook\n" {
		t.Fatalf("b reads after admission: %v", err)
	}
	// merge drivers decrypt hybrid sides and re-encrypt to the pinned set
	a.write(t, "MEMORY.md", a.read(t, "MEMORY.md")+"- [fromA](a.md) hook\n")
	a.write(t, "notes/shared.md", "line1 A\nline2\nline3\n")
	a.commit(t, "memsync: 2 files")
	b.write(t, "MEMORY.md", b.read(t, "MEMORY.md")+"- [fromB](b.md) hook\n")
	b.write(t, "notes/shared.md", "line1\nline2\nline3 B\n")
	b.commit(t, "memsync: 2 files")
	if _, err := a.repo.Run("push", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	b.repo.Run("fetch", "-q", "origin", "main")
	if err := b.repo.RebaseOnto("main", "test"); err != nil {
		t.Fatal(err)
	}
	if got := b.read(t, "notes/shared.md"); got != "line1 A\nline2\nline3 B\n" {
		t.Errorf("text merge: %q", got)
	}
	if m := b.read(t, "MEMORY.md"); !strings.Contains(m, "fromA") || !strings.Contains(m, "fromB") {
		t.Errorf("index merge: %q", m)
	}
	if !IsHybridCiphertext([]byte(b.blob(t, "HEAD", "notes/shared.md"))) {
		t.Error("merged result stays hybrid")
	}
}

func TestPQ_ClassicRefusedAndMixedSetRefused(t *testing.T) {
	gitEnv(t)
	a := newHostPQ(t, bareRemote(t), true)
	a.write(t, "n.md", "x\n")
	a.commit(t, "memsync: 1 files")
	_, cl := newIdentity(t, "c")
	if err := AddRecipient(a.repo, cl); err == nil || !strings.Contains(err.Error(), "post-quantum") {
		t.Fatalf("classic add under pq: %v", err)
	}
	if err := AddRecipientLabelled(a.repo, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHcbV3DnP0R7ZqzuZXlzJ4W0eKgQh8WZ1xX3nYF8mO6c", "h2"); err == nil {
		t.Fatal("ssh add under pq must be refused")
	}
	// a recipients file edited upstream to a mixed set is unreviewed and cannot be encrypted to
	rec := a.read(t, ".age-recipients") + cl + "\n"
	a.write(t, ".age-recipients", rec)
	if _, err := Encrypt([]byte("x"), mustRecipients(t, rec)); err == nil {
		t.Error("mixed set must not encrypt")
	}
	if u, _ := Unreviewed(a.repo); !u {
		t.Error("the changed set is unreviewed")
	}
	// classic repos are unchanged: a hybrid key is refused there
	c := newHost(t, bareRemote(t), true)
	c.write(t, "n.md", "x\n")
	c.commit(t, "memsync: 1 files")
	_, pq := newPQIdentity(t, "p")
	if err := AddRecipient(c.repo, pq); err == nil {
		t.Error("hybrid into classic must be refused")
	}
	if err := AddRecipient(c.repo, cl); err != nil {
		t.Errorf("classic add in classic repo: %v", err)
	}
}

func TestPQ_AcceptJoinWithHybridKey(t *testing.T) {
	gitEnv(t)
	remote := bareRemote(t)
	a := newHostPQ(t, remote, true)
	a.write(t, "n.md", "x\n")
	a.commit(t, "memsync: 1 files")
	if err := LabelSelf(a.repo, "vm-1"); err != nil {
		t.Fatal(err)
	}
	a.repo.Run("push", "origin", "main")
	b := newHostPQ(t, remote, false)
	j, res, err := RequestJoin(b.repo, "vm-2", joinNow)
	if err != nil || res != JoinRequested {
		t.Fatalf("request: %v %v", res, err)
	}
	if b.blob(t, "HEAD", "joins/vm-2.txt") != j.Format() {
		t.Error("request blob")
	}
	if err := PushGuardJoinRequest(b.repo, "main"); err != nil {
		t.Fatalf("join request guard: %v", err)
	}
	b.repo.Run("push", "origin", "main")
	a.repo.Run("fetch", "-q", "origin", "main")
	a.repo.Run("merge", "--ff-only", "-q", "origin/main")
	if err := AcceptJoin(a.repo, "vm-2", j.Fingerprint, AcceptOpts{OwnLabel: "vm-1"}); err != nil {
		t.Fatal(err)
	}
	if l, _ := ListLabels(a.repo.Dir); len(l) != 2 {
		t.Errorf("labels: %v", l)
	}
	if !IsHybridCiphertext([]byte(a.blob(t, "HEAD", "n.md"))) {
		t.Error("re-encrypted to the hybrid set")
	}
	// a classic join request is refused under pq
	_, cl := newIdentity(t, "c")
	cj := Join{Label: "vm-3", Recipient: cl, Fingerprint: JoinFingerprint(cl), Requested: joinNow}
	a.write(t, "joins/vm-3.txt", cj.Format())
	a.commit(t, "join")
	if err := AcceptJoin(a.repo, "vm-3", cj.Fingerprint, AcceptOpts{OwnLabel: "vm-1", AllowUnlabelled: true}); err == nil {
		t.Error("classic join under pq must be refused")
	}
}

func TestPQ_PushGuardRefusesClassicCiphertextUnderPQ(t *testing.T) {
	gitEnv(t)
	a := newHostPQ(t, bareRemote(t), true)
	a.write(t, "n.md", "ok\n")
	a.commit(t, "memsync: 1 files")
	// forge a classic blob into the next commit
	_, cl := newIdentity(t, "c")
	ct, _ := Encrypt([]byte("legacy"), mustRecipients(t, cl))
	oid, err := a.repo.RunStdin(ct, "hash-object", "-w", "--no-filters", "--stdin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.repo.Run("update-index", "--add", "--cacheinfo", "100644,"+strings.TrimSpace(oid)+",legacy.md"); err != nil {
		t.Fatal(err)
	}
	a.repo.Commit("legacy")
	if err := PushGuard(a.repo, "main"); err == nil {
		t.Fatal("classic ciphertext must not be pushed from a pq repo")
	}
}

func TestPQ_IdentityNeverInRecipientsOrErrors(t *testing.T) {
	p, _ := newPQIdentity(t, "id")
	data, _ := os.ReadFile(p)
	var secret string
	for _, l := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(l, "AGE-SECRET-KEY-PQ-1") {
			secret = l
		}
	}
	if secret == "" {
		t.Fatal("identity file holds the secret")
	}
	if _, err := age.ParseHybridIdentity(secret); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	_, err := IdentityRecipient(filepath.Join(t.TempDir(), "missing"))
	buf.WriteString(err.Error())
	if strings.Contains(buf.String(), secret) {
		t.Error("error text must not carry the secret")
	}
}

// forgeClassic commits a classic-ciphertext blob as legacy.md on h's branch.
func forgeClassic(t *testing.T, h *host) {
	t.Helper()
	_, cl := newIdentity(t, "c")
	ct, _ := Encrypt([]byte("legacy"), mustRecipients(t, cl))
	oid, err := h.repo.RunStdin(ct, "hash-object", "-w", "--no-filters", "--stdin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.repo.Run("update-index", "--add", "--cacheinfo", "100644,"+strings.TrimSpace(oid)+",legacy.md"); err != nil {
		t.Fatal(err)
	}
	h.repo.Commit("legacy")
}

// A clone whose memage.scheme went missing is still pq if its recipients are:
// the guard must not fall back to accepting classic ciphertext.
func TestPQ_SchemeOfFallsBackToTheRecipientsFile(t *testing.T) {
	gitEnv(t)
	a := newHostPQ(t, bareRemote(t), true)
	a.write(t, "n.md", "ok\n")
	a.commit(t, "memsync: 1 files")
	if _, err := a.repo.Run("config", "--unset", SchemeConfigKey); err != nil {
		t.Fatal(err)
	}
	if got := SchemeOf(a.repo); got != SchemePQ {
		t.Fatalf("SchemeOf without memage.scheme but with a hybrid recipients file = %q, want pq", got)
	}
	forgeClassic(t, a)
	if err := PushGuard(a.repo, "main"); err == nil {
		t.Fatal("classic ciphertext must not be pushed from a repo with hybrid recipients")
	}
	// a classic repo stays classic
	c := newHost(t, bareRemote(t), true)
	if got := SchemeOf(c.repo); got != SchemeClassic {
		t.Errorf("classic repo reads as %q", got)
	}
}
