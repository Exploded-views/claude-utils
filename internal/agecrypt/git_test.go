package agecrypt

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/testenv"
)

// gitEnv isolates git from the real HOME and config.
func gitEnv(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
}

func bareRemote(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "origin.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", "-b", "main", p).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	return p
}

// host is one encrypted clone with its own identity.
type host struct {
	repo   *gitwt.Repo
	idPath string
	rcpt   string
}

// newHost provisions a clone the way init does: configure before checkout. The
// first host (first=true) creates the recipients file and attributes.
func newHost(t *testing.T, remote string, first bool, extra ...string) *host {
	t.Helper()
	ms, mm := testenv.Binaries(t)
	idPath, rcpt := newIdentity(t, "id")
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
	if err := Configure(repo, Opts{MemsyncExe: ms, MemmergeExe: mm, IdentityPath: idPath}); err != nil {
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
	} else if repo.HasHead() {
		if _, err := repo.Run("checkout", "-q", "main"); err != nil {
			t.Fatal(err)
		}
	}
	return &host{repo: repo, idPath: idPath, rcpt: rcpt}
}

func (h *host) write(t *testing.T, rel, body string) {
	t.Helper()
	p := filepath.Join(h.repo.Dir, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(p), 0o700)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (h *host) read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(h.repo.Dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (h *host) commit(t *testing.T, msg string) {
	t.Helper()
	if _, err := h.repo.Run("add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.repo.Commit(msg); err != nil {
		t.Fatal(err)
	}
}

func (h *host) dirty(t *testing.T) string {
	t.Helper()
	out, err := h.repo.Run("status", "--porcelain")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out)
}

func (h *host) blob(t *testing.T, rev, rel string) string {
	t.Helper()
	out, err := h.repo.Run("cat-file", "blob", rev+":"+rel)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestGit_StoresCiphertextAndStaysClean(t *testing.T) {
	gitEnv(t)
	remote := bareRemote(t)
	a := newHost(t, remote, true)
	a.write(t, "MEMORY.md", "- [one](notes/one.md) hook\n")
	a.write(t, "notes/one.md", "the secret note text\n")
	a.commit(t, "memsync: 2 files")
	if !IsCiphertext([]byte(a.blob(t, "HEAD", "notes/one.md"))) || !IsCiphertext([]byte(a.blob(t, "HEAD", "MEMORY.md"))) {
		t.Fatal("blobs in the repo must be ciphertext")
	}
	if got := a.blob(t, "HEAD", ".age-recipients"); !strings.Contains(got, a.rcpt) {
		t.Error("recipients must stay readable")
	}
	if a.read(t, "notes/one.md") != "the secret note text\n" {
		t.Error("work-tree must hold plaintext")
	}
	if d := a.dirty(t); d != "" {
		t.Fatalf("dirty after commit: %s", d)
	}
	// Re-checkout from scratch, and with the cache gone, must stay clean.
	for _, drop := range []bool{false, true} {
		if drop {
			os.RemoveAll(filepath.Join(a.repo.Dir, ".git", "memage-cache"))
		}
		os.Remove(filepath.Join(a.repo.Dir, "notes/one.md"))
		os.Remove(filepath.Join(a.repo.Dir, "MEMORY.md"))
		if _, err := a.repo.Run("checkout", "-f", "HEAD", "--", "."); err != nil {
			t.Fatal(err)
		}
		if _, err := a.repo.Run("update-index", "--really-refresh"); err != nil {
			t.Fatal(err)
		}
		if d := a.dirty(t); d != "" {
			t.Fatalf("dirty after re-checkout (cache dropped=%v): %s", drop, d)
		}
	}
	// Touching a file (mtime change) must not produce a diff either.
	now := a.read(t, "notes/one.md")
	a.write(t, "notes/one.md", now)
	if d := a.dirty(t); d != "" {
		t.Fatalf("dirty after rewrite with same content: %s", d)
	}
}

func TestGit_SecondHostWithKeyReadsAfterRecipientAdd(t *testing.T) {
	gitEnv(t)
	remote := bareRemote(t)
	a := newHost(t, remote, true)
	a.write(t, "MEMORY.md", "- first\n")
	a.commit(t, "memsync: 1 files")
	if _, err := a.repo.Run("push", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	b := newHost(t, remote, false) // not a recipient yet
	if err := CheckKeyed(b.repo); err == nil {
		t.Fatal("a clone whose key is not a recipient must be keyless")
	}
	// Work-tree holds ciphertext, and it is clean.
	if !IsCiphertext([]byte(b.read(t, "MEMORY.md"))) || b.dirty(t) != "" {
		t.Fatal("keyless clone should hold ciphertext and be clean")
	}
	// Appending to the ciphertext must be refused by the filter, not stored.
	p := filepath.Join(b.repo.Dir, "MEMORY.md")
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("- smuggled plaintext\n")
	f.Close()
	if _, err := b.repo.Run("add", "-A"); err == nil {
		t.Error("git add of an edited ciphertext file must fail")
	}
	b.repo.Run("reset", "-q")
	b.repo.Run("checkout", "-q", "--", "MEMORY.md")

	if err := AddRecipient(a.repo, b.rcpt); err != nil {
		t.Fatal(err)
	}
	if _, err := a.repo.Run("push", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.repo.Run("pull", "-q", "--rebase", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	if err := CheckKeyed(b.repo); err != nil {
		t.Fatalf("after recipients add, b can read: %v", err)
	}
	if b.read(t, "MEMORY.md") != "- first\n" {
		t.Errorf("b sees %q", b.read(t, "MEMORY.md"))
	}
}

func TestGit_RemovedRecipientCannotReadNewBlobs(t *testing.T) {
	gitEnv(t)
	remote := bareRemote(t)
	a := newHost(t, remote, true)
	b := newHost(t, remote, false) // empty remote: clone gives an unborn repo
	_ = b
	// Rebuild b as a recipient from the start.
	idB, rcptB := newIdentity(t, "b")
	if err := AddRecipient(a.repo, rcptB); err != nil {
		t.Fatal(err)
	}
	a.write(t, "n.md", "visible to both\n")
	a.commit(t, "memsync: 1 files")
	old := a.blob(t, "HEAD", "n.md")
	if err := RemoveRecipient(a.repo, rcptB); err != nil {
		t.Fatal(err)
	}
	a.write(t, "n.md", "after revocation\n")
	a.commit(t, "memsync: 1 files")
	idsB, err := LoadIdentities(idB)
	if err != nil {
		t.Fatal(err)
	}
	if pt, err := Decrypt([]byte(old), idsB); err != nil || string(pt) != "visible to both\n" {
		t.Errorf("history stays readable to the removed key: %q %v", pt, err)
	}
	if _, err := Decrypt([]byte(a.blob(t, "HEAD", "n.md")), idsB); err == nil {
		t.Error("removed key must not read new blobs")
	}
	// The unchanged file was re-encrypted too: HEAD's other blobs are closed to b.
	if err := RemoveRecipient(a.repo, a.rcpt); err == nil {
		t.Error("removing the last recipient must be refused")
	}
	if err := AddRecipient(a.repo, a.rcpt); err == nil {
		t.Error("adding a duplicate must be refused")
	}
	if err := AddRecipient(a.repo, "not-a-key"); err == nil {
		t.Error("garbage must be refused")
	}
}

func TestGit_PushGuardCatchesPlaintextFromMisconfiguredClone(t *testing.T) {
	gitEnv(t)
	remote := bareRemote(t)
	a := newHost(t, remote, true)
	a.write(t, "n.md", "ok\n")
	a.commit(t, "memsync: 1 files")
	if err := PushGuard(a.repo, "main"); err != nil {
		t.Fatalf("clean encrypted history must pass: %v", err)
	}
	a.repo.Run("push", "origin", "main")
	// A clone that lost its filter config still carries .age-recipients, so it
	// is held to the encrypted rules.
	a.repo.Run("config", "--unset-all", "filter.memage.required")
	a.repo.Run("config", "--unset-all", "filter.memage.process")
	a.write(t, "leak.md", "plaintext secret\n")
	a.repo.Run("add", "-A")
	a.repo.Commit("oops")
	if !Enabled(a.repo) {
		t.Fatal("a clone with .age-recipients is encrypted even without filter config")
	}
	err := PushGuard(a.repo, "main")
	if err == nil || !strings.Contains(err.Error(), "not age ciphertext") {
		t.Fatalf("guard must refuse, got %v", err)
	}
	if strings.Contains(err.Error(), "leak.md") {
		t.Error("the message must not name the file")
	}
}

func TestGit_MemoryMdAndNoteMergesThroughRealDrivers(t *testing.T) {
	gitEnv(t)
	remote := bareRemote(t)
	a := newHost(t, remote, true)
	a.write(t, "MEMORY.md", "- [base](base.md) hook\n")
	a.write(t, "notes/shared.md", "line1\nline2\nline3\n")
	a.commit(t, "memsync: 2 files")
	a.repo.Run("push", "origin", "main")
	if err := AddRecipient(a.repo, ""); err == nil {
		t.Fatal("expected failure on empty")
	}
	b := newHost(t, remote, false)
	// b is keyless; make it a recipient first.
	if err := AddRecipient(a.repo, b.rcpt); err != nil {
		t.Fatal(err)
	}
	a.repo.Run("push", "origin", "main")
	b.repo.Run("pull", "-q", "--rebase", "origin", "main")

	// Both append to MEMORY.md and edit different lines of the note: clean merges.
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
		un, _ := b.repo.UnmergedPaths()
		t.Fatalf("rebase through drivers: %v (unmerged %v)\n%s", err, un, b.read(t, "MEMORY.md"))
	}
	mem := b.read(t, "MEMORY.md")
	if !strings.Contains(mem, "fromA") || !strings.Contains(mem, "fromB") {
		t.Errorf("MEMORY.md union lost an entry:\n%s", mem)
	}
	if got := b.read(t, "notes/shared.md"); got != "line1 A\nline2\nline3 B\n" {
		t.Errorf("note merge: %q", got)
	}
	if !IsCiphertext([]byte(b.blob(t, "HEAD", "MEMORY.md"))) || !IsCiphertext([]byte(b.blob(t, "HEAD", "notes/shared.md"))) {
		t.Fatal("merged results must be stored as ciphertext")
	}
	if d := b.dirty(t); d != "" {
		t.Fatalf("dirty after merge: %s", d)
	}
	b.repo.Run("push", "origin", "main")

	// Same-line conflict: readable markers in the work-tree, ciphertext in the index.
	a.repo.Run("pull", "-q", "--rebase", "origin", "main")
	a.write(t, "notes/shared.md", "ALPHA\nline2\nline3 B\n")
	a.commit(t, "memsync: 1 files")
	b.write(t, "notes/shared.md", "BETA\nline2\nline3 B\n")
	b.commit(t, "memsync: 1 files")
	a.repo.Run("push", "origin", "main")
	b.repo.Run("fetch", "-q", "origin", "main")
	if err := b.repo.RebaseOnto("main", "test"); err == nil {
		t.Fatal("expected a conflict")
	}
	got := b.read(t, "notes/shared.md")
	if !strings.Contains(got, "<<<<<<<") || !strings.Contains(got, "ALPHA") || !strings.Contains(got, "BETA") {
		t.Errorf("work-tree should show plaintext conflict markers:\n%s", got)
	}
	for _, stage := range []string{":1:", ":2:", ":3:"} {
		out, err := b.repo.Run("show", stage+"notes/shared.md")
		if err != nil || !IsCiphertext([]byte(out)) {
			t.Errorf("index stage %s must be ciphertext (err %v)", stage, err)
		}
	}
	b.repo.RebaseAbort()
	if d := b.dirty(t); d != "" {
		t.Errorf("dirty after abort: %s", d)
	}
}
