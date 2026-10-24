package sync

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	stdsync "sync"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/testenv"
)

func ageEnv(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	t.Setenv("HOME", t.TempDir()) // never the real home
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	origin := filepath.Join(t.TempDir(), "origin.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", "-b", "main", origin).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	return origin
}

type ageHost struct {
	t     *testing.T
	repo  *gitwt.Repo
	roots Roots
	loop  *Loop
	rcpt  string
}

// newAgeHost provisions a daemon-equivalent host the way init does.
func newAgeHost(t *testing.T, origin, name string, first bool) *ageHost {
	t.Helper()
	ms, mm := testenv.Binaries(t)
	root := t.TempDir()
	idPath := filepath.Join(root, "identity")
	rcpt, err := agecrypt.GenerateIdentity(idPath)
	if err != nil {
		t.Fatal(err)
	}
	syncDir := filepath.Join(root, "sync")
	repo := gitwt.New(syncDir)
	if first {
		if err := repo.Init("main"); err != nil {
			t.Fatal(err)
		}
		if err := repo.SetRemote(origin); err != nil {
			t.Fatal(err)
		}
	} else if err := repo.CloneNoCheckout(origin); err != nil {
		t.Fatal(err)
	}
	if err := agecrypt.Configure(repo, agecrypt.Opts{MemsyncExe: ms, MemmergeExe: mm, IdentityPath: idPath}); err != nil {
		t.Fatal(err)
	}
	if first {
		agecrypt.WriteRecipients(syncDir, []string{rcpt})
		agecrypt.WriteAttributes(syncDir)
		os.WriteFile(filepath.Join(syncDir, ".gitignore"), []byte("config.json\ndaemon.pid\n.state/\ndenylist.txt\n*.tmp\n*.tmp.*\n*.from-remote-*\n"), 0o600)
	} else {
		if _, err := repo.Run("checkout", "-q", "main"); err != nil {
			t.Fatal(err)
		}
	}
	if err := agecrypt.PinCurrent(repo); err != nil {
		t.Fatal(err)
	}
	claude := filepath.Join(root, "claude", "projects")
	os.MkdirAll(claude, 0o700)
	cfg := config.Defaults()
	cfg.SyncDir, cfg.ClaudeProjectsDir = syncDir, claude
	return &ageHost{t: t, repo: repo, rcpt: rcpt,
		roots: Roots{Claude: claude, Mirror: filepath.Join(syncDir, "projects")},
		loop:  &Loop{Cfg: cfg, Branch: "main", Hostname: "host-" + name}}
}

func (h *ageHost) edit(name, content string) {
	h.t.Helper()
	writeMem(h.t, h.roots.Claude, "p1", name, content)
	if err := CopyToMirror(h.roots, "p1", name); err != nil {
		h.t.Fatal(err)
	}
}

func (h *ageHost) claude(name string) string {
	b, _ := os.ReadFile(filepath.Join(h.roots.Claude, "p1", "memory", name))
	return string(b)
}

func (h *ageHost) flushRetry() error {
	var err error
	for i := 0; i < 30; i++ {
		if err = h.loop.flush(h.repo, h.roots, true); err == nil {
			return nil
		}
		if !strings.Contains(err.Error(), "push") {
			return err
		}
	}
	return err
}

// join makes h a recipient via a (already keyed) and brings it up to date.
func (h *ageHost) join(a *ageHost) {
	h.t.Helper()
	if err := agecrypt.AddRecipient(a.repo, h.rcpt); err != nil {
		h.t.Fatal(err)
	}
	if err := a.flushRetry(); err != nil {
		h.t.Fatal(err)
	}
	// Keyless until the pull brings the recipients; the daemon pulls anyway.
	if err := h.loop.flush(h.repo, h.roots, true); !errors.Is(err, agecrypt.ErrKeyless) {
		h.t.Fatalf("first cycle of a host that is not yet a recipient: %v", err)
	}
	if err := agecrypt.AcceptRecipients(h.repo); err != nil { // the owner reviewed it
		h.t.Fatal(err)
	}
	if err := h.flushRetry(); err != nil {
		h.t.Fatal(err)
	}
}

func allBlobs(t *testing.T, bare string) map[string][]byte {
	t.Helper()
	r := gitwt.New(bare)
	out, err := r.Run("rev-list", "--objects", "--all")
	if err != nil {
		t.Fatal(err)
	}
	blobs := map[string][]byte{}
	for _, l := range strings.Split(out, "\n") {
		f := strings.SplitN(l, " ", 2)
		if len(f) != 2 {
			continue
		}
		typ, _ := r.Run("cat-file", "-t", f[0])
		if strings.TrimSpace(typ) == "blob" {
			b, _ := r.BlobContents([]string{f[0]})
			blobs[f[1]] = b[0]
		}
	}
	return blobs
}

func TestAge_ConcurrentWritersKeepEverythingAndStoreNoPlaintext(t *testing.T) {
	origin := ageEnv(t)
	a := newAgeHost(t, origin, "aaa", true)
	a.edit("seed.md", "PLAINMARK seed\n")
	if err := a.flushRetry(); err != nil {
		t.Fatal(err)
	}
	b := newAgeHost(t, origin, "bbb", false)
	c := newAgeHost(t, origin, "ccc", false)
	b.join(a)
	c.join(a)
	b.loop.flush(b.repo, b.roots, true) // pulls c's admission; the owner reviews it
	if err := agecrypt.AcceptRecipients(b.repo); err != nil {
		t.Fatal(err)
	}

	const rounds = 5
	hosts := []*ageHost{a, b, c}
	var wg stdsync.WaitGroup
	errs := make([]error, len(hosts))
	for i, h := range hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				note := fmt.Sprintf("%s-%d.md", h.loop.Hostname, r)
				idx := h.claude("MEMORY.md") + fmt.Sprintf("- [%s](%s) PLAINMARK hook\n", note, note)
				// Claude-side edits (not t.Fatal: this is a goroutine).
				d := filepath.Join(h.roots.Claude, "p1", "memory")
				os.MkdirAll(d, 0o700)
				os.WriteFile(filepath.Join(d, note), []byte("PLAINMARK "+note+"\n"), 0o600)
				os.WriteFile(filepath.Join(d, "MEMORY.md"), []byte(idx), 0o600)
				for _, n := range []string{note, "MEMORY.md"} {
					if err := CopyToMirror(h.roots, "p1", n); err != nil {
						errs[i] = err
						return
					}
				}
				if err := h.flushRetry(); err != nil {
					errs[i] = err
					return
				}
			}
		}()
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("host %d: %v", i, e)
		}
	}
	for _, h := range hosts { // settle: everyone pulls everyone's last push
		if err := h.flushRetry(); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range hosts {
		mem := h.claude("MEMORY.md")
		for _, o := range hosts {
			for r := 0; r < rounds; r++ {
				if !strings.Contains(mem, fmt.Sprintf("host-%s", strings.TrimPrefix(o.loop.Hostname, "host-"))+fmt.Sprintf("-%d.md", r)) {
					t.Fatalf("%s: MEMORY.md lost %s round %d:\n%s", h.loop.Hostname, o.loop.Hostname, r, mem)
				}
			}
		}
	}
	// Every commit survived: one "memsync: 2 files" per round per host.
	log, _ := gitwt.New(origin).Run("log", "--format=%an|%ae|%s", "main")
	if n := strings.Count(log, "|memsync: 2 files"); n != rounds*len(hosts) {
		t.Errorf("commits with 2 files = %d, want %d\n%s", n, rounds*len(hosts), log)
	}
	for _, h := range hosts {
		if strings.Contains(log, strings.TrimPrefix(h.loop.Hostname, "host-")) || strings.Contains(log, h.loop.Hostname) {
			t.Error("commit metadata leaks a host name")
		}
	}
	if strings.Contains(log, ".md") {
		t.Error("commit messages must not name notes")
	}
	for path, b := range allBlobs(t, origin) {
		if agecrypt.Exempt(path) {
			continue
		}
		if !agecrypt.IsCiphertext(b) || bytes.Contains(b, []byte("PLAINMARK")) {
			t.Errorf("origin holds a plaintext blob at a note path")
		}
	}
	for _, h := range hosts {
		if d, _ := h.repo.Run("status", "--porcelain"); strings.TrimSpace(d) != "" {
			t.Errorf("%s: dirty clone:\n%s", h.loop.Hostname, d)
		}
	}
}

func TestAge_KeylessHostCommitsAndPropagatesNothing(t *testing.T) {
	origin := ageEnv(t)
	a := newAgeHost(t, origin, "aaa", true)
	a.edit("n.md", "secret\n")
	if err := a.flushRetry(); err != nil {
		t.Fatal(err)
	}
	k := newAgeHost(t, origin, "kkk", false) // not a recipient
	k.edit("local.md", "unsent\n")
	if ok, err := CommitMirror(k.repo, k.loop.Cfg, "x"); ok || !errors.Is(err, agecrypt.ErrKeyless) {
		t.Fatalf("CommitMirror = %v, %v; want ErrKeyless", ok, err)
	}
	if err := k.loop.flush(k.repo, k.roots, true); !errors.Is(err, agecrypt.ErrKeyless) {
		t.Fatalf("flush = %v", err)
	}
	if n, _ := k.repo.Run("rev-list", "--count", "origin/main..HEAD"); strings.TrimSpace(n) != "0" {
		t.Error("keyless host committed")
	}
	if got := k.claude("n.md"); got != "" {
		t.Errorf("ciphertext reached the Claude tree: %q", got)
	}
	// Once admitted it catches up: plaintext arrives in the Claude tree.
	if err := agecrypt.AddRecipient(a.repo, k.rcpt); err != nil {
		t.Fatal(err)
	}
	if err := a.flushRetry(); err != nil {
		t.Fatal(err)
	}
	k.loop.flush(k.repo, k.roots, true) // keyless pull picks the recipients up
	agecrypt.AcceptRecipients(k.repo)
	k.loop.pullTick(k.repo, k.roots)
	if got := k.claude("n.md"); got != "secret\n" {
		t.Errorf("after admission the note should reach the Claude tree, got %q", got)
	}
}

func TestAge_UnreviewedRecipientBlocksCommitPushAndEncryption(t *testing.T) {
	origin := ageEnv(t)
	a := newAgeHost(t, origin, "aaa", true)
	a.edit("n.md", "one\n")
	if err := a.flushRetry(); err != nil {
		t.Fatal(err)
	}
	// An attacker with write access adds a key to .age-recipients upstream.
	evilID := filepath.Join(t.TempDir(), "evil-identity")
	evilRcpt, err := agecrypt.GenerateIdentity(evilID)
	if err != nil {
		t.Fatal(err)
	}
	evil := gitwt.New(filepath.Join(t.TempDir(), "evil"))
	if err := evil.CloneNoCheckout(origin); err != nil {
		t.Fatal(err)
	}
	mustGit(t, evil, "checkout", "-q", "main")
	cur, _ := os.ReadFile(filepath.Join(evil.Dir, agecrypt.RecipientsFile))
	os.WriteFile(filepath.Join(evil.Dir, agecrypt.RecipientsFile), append(cur, []byte(evilRcpt+"\n")...), 0o600)
	mustGit(t, evil, "add", agecrypt.RecipientsFile)
	mustGit(t, evil, "commit", "-q", "-m", "x")
	mustGit(t, evil, "push", "-q", "origin", "main")
	originBefore := strings.TrimSpace(mustGit(t, gitwt.New(origin), "rev-parse", "main"))

	a.edit("n.md", "two\n")
	err = a.loop.flush(a.repo, a.roots, true)
	if !errors.Is(err, agecrypt.ErrRecipientsUnreviewed) {
		t.Fatalf("flush = %v, want ErrRecipientsUnreviewed", err)
	}
	if got := strings.TrimSpace(mustGit(t, gitwt.New(origin), "rev-parse", "main")); got != originBefore {
		t.Error("a push happened while the recipients were unreviewed")
	}
	if ok, err := CommitMirror(a.repo, a.loop.Cfg, "x"); ok || !errors.Is(err, agecrypt.ErrRecipientsUnreviewed) {
		t.Errorf("CommitMirror = %v, %v", ok, err)
	}
	// Even a forced add never encrypts to the new key: the filter uses the pin.
	mustGit(t, a.repo, "add", "-A")
	blob := mustGit(t, a.repo, "cat-file", "blob", ":projects/p1/memory/n.md")
	evilIDs, err := agecrypt.LoadIdentities(evilID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agecrypt.Decrypt([]byte(blob), evilIDs); err == nil {
		t.Error("an unreviewed recipient received ciphertext")
	}
	mustGit(t, a.repo, "reset", "-q")

	// After accept, sync resumes.
	if err := agecrypt.AcceptRecipients(a.repo); err != nil {
		t.Fatal(err)
	}
	if err := a.flushRetry(); err != nil {
		t.Fatalf("after accept: %v", err)
	}
	if got := strings.TrimSpace(mustGit(t, gitwt.New(origin), "rev-parse", "main")); got == originBefore {
		t.Error("sync did not resume after accept")
	}
}

func TestAge_LocalRecipientsAddDoesNotTripTheGuard(t *testing.T) {
	origin := ageEnv(t)
	a := newAgeHost(t, origin, "aaa", true)
	a.edit("n.md", "one\n")
	if err := a.flushRetry(); err != nil {
		t.Fatal(err)
	}
	r, _ := agecrypt.GenerateIdentity(filepath.Join(t.TempDir(), "r"))
	if err := agecrypt.AddRecipient(a.repo, r); err != nil {
		t.Fatal(err)
	}
	if u, _ := agecrypt.Unreviewed(a.repo); u {
		t.Fatal("a local recipients add must update the pin")
	}
	a.edit("n.md", "two\n")
	if err := a.flushRetry(); err != nil {
		t.Fatalf("sync after local add: %v", err)
	}
}

func TestAge_ScrubScansPlaintextNotCiphertext(t *testing.T) {
	origin := ageEnv(t)
	a := newAgeHost(t, origin, "aaa", true)
	a.edit("fine.md", "harmless\n")
	a.edit("leak.md", "token "+fakeSecret()+"\n")
	if _, err := CommitMirror(a.repo, a.loop.Cfg, "x"); err != nil {
		t.Fatal(err)
	}
	files := committedFiles(t, a.repo)
	if files["projects/p1/memory/leak.md"] || !files["projects/p1/memory/fine.md"] {
		t.Errorf("scrub must hold back the secret even when encrypted: %v", files)
	}
}
