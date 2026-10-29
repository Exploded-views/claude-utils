package changes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
)

func (f *fixture) encrypt(plain string, rcpt *agecrypt.Recipients) string {
	f.t.Helper()
	ct, err := agecrypt.Encrypt([]byte(plain), rcpt)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(ct)
}

// In an encrypted repo a recipients change re-encrypts every blob without
// touching a note: the report drops those, keeps a real edit, and keeps a file
// it cannot decrypt (in doubt, report it).
func TestReencryptedNotesAreNotReported(t *testing.T) {
	f := newFixture(t)
	idPath := filepath.Join(t.TempDir(), "id")
	rcptLine, err := agecrypt.GenerateIdentity(idPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.repo.ConfigSet(agecrypt.IdentityConfigKey, idPath); err != nil {
		t.Fatal(err)
	}
	mine, err := agecrypt.ParseRecipients([]byte(rcptLine + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	otherLine, err := agecrypt.GenerateIdentity(filepath.Join(t.TempDir(), "other"))
	if err != nil {
		t.Fatal(err)
	}
	others, _ := agecrypt.ParseRecipients([]byte(otherLine + "\n"))

	same, edited, locked := mem(appHash, "same.md"), mem(appHash, "edited.md"), mem(appHash, "locked.md")
	first := f.commit(map[string]string{
		agecrypt.RecipientsFile: rcptLine + "\n",
		same:                    f.encrypt("unchanged text\n", mine),
		edited:                  f.encrypt("before\n", mine),
		locked:                  f.encrypt("not for me\n", others),
	})
	f.commit(map[string]string{
		same:   f.encrypt("unchanged text\n", mine),
		edited: f.encrypt("after\n", mine),
		locked: f.encrypt("not for me\n", others),
	})
	if a, _ := os.ReadFile(filepath.Join(f.repo.Dir, filepath.FromSlash(same))); strings.Contains(string(a), "unchanged") {
		t.Fatal("fixture note is not ciphertext")
	}

	res, err := Compute(f.opts(first))
	if err != nil {
		t.Fatal(err)
	}
	if find(res, "same.md") != nil {
		t.Errorf("a re-encrypted, unchanged note was reported: %+v", res.Changes)
	}
	if find(res, "edited.md") == nil {
		t.Errorf("a really edited note was dropped: %+v", res.Changes)
	}
	if find(res, "locked.md") == nil {
		t.Errorf("a note this host cannot decrypt must still be reported: %+v", res.Changes)
	}
	if res.Total != 2 {
		t.Errorf("Total = %d, want 2", res.Total)
	}
}

// A plain repo reports a note whose text is identical only when git says so;
// dropReencrypted must not touch it.
func TestDropReencryptedLeavesPlainRepoAlone(t *testing.T) {
	f := newFixture(t)
	in := []Change{{Path: "a", Status: "changed"}}
	if out := dropReencrypted(f.repo, false, "x", "y", in); len(out) != 1 {
		t.Errorf("plain repo: %v", out)
	}
	// Only "changed" files are compared; an added file never reaches git.
	added := []Change{{Path: "a", Status: "added"}}
	if out := dropReencrypted(f.repo, true, "x", "y", added); len(out) != 1 {
		t.Errorf("added file: %v", out)
	}
}

// A repo with .age-recipients only in HEAD (not in the work-tree) is still
// encrypted: the answer comes from the prefetched HEAD lookup.
func TestEncryptedOnlyAtHeadStillDropsReencrypted(t *testing.T) {
	f := newFixture(t)
	idPath := filepath.Join(t.TempDir(), "id")
	rcptLine, err := agecrypt.GenerateIdentity(idPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.repo.ConfigSet(agecrypt.IdentityConfigKey, idPath)
	mine, _ := agecrypt.ParseRecipients([]byte(rcptLine + "\n"))
	note := mem(appHash, "n.md")
	first := f.commit(map[string]string{agecrypt.RecipientsFile: rcptLine + "\n", note: f.encrypt("t\n", mine)})
	f.commit(map[string]string{note: f.encrypt("t\n", mine)})
	// Remove the file from the work-tree only; HEAD still has it.
	if err := os.Remove(filepath.Join(f.repo.Dir, agecrypt.RecipientsFile)); err != nil {
		t.Fatal(err)
	}
	res, err := Compute(f.opts(first))
	if err != nil || res.Total != 0 {
		t.Fatalf("re-encrypted note reported with recipients known only at HEAD: %+v %v", res, err)
	}
}

// AdvanceMarker never moves the marker backwards.
func TestAdvanceMarkerNeverGoesBackwards(t *testing.T) {
	f := newFixture(t)
	h1 := f.commit(map[string]string{mem(appHash, "a.md"): "1\n"})
	h2 := f.commit(map[string]string{mem(appHash, "b.md"): "2\n"})
	marker := filepath.Join(t.TempDir(), "m", "marker")
	read := func() string { b, _ := os.ReadFile(marker); return markerLine(b) }

	if err := AdvanceMarker(f.repo, marker, h1); err != nil || read() != h1 { // no marker yet
		t.Fatalf("first write: %q %v", read(), err)
	}
	if err := AdvanceMarker(f.repo, marker, h2); err != nil || read() != h2 { // forward
		t.Fatalf("forward: %q %v", read(), err)
	}
	if err := AdvanceMarker(f.repo, marker, h1); err != nil || read() != h2 { // backward: kept
		t.Fatalf("backward moved the marker: %q %v", read(), err)
	}
	if err := AdvanceMarker(f.repo, marker, h2); err != nil || read() != h2 { // same
		t.Fatalf("same: %q %v", read(), err)
	}
	// A marker naming a commit that no longer exists is replaced.
	if err := os.WriteFile(marker, []byte(strings.Repeat("a", 40)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AdvanceMarker(f.repo, marker, h1); err != nil || read() != h1 {
		t.Fatalf("lost commit: %q %v", read(), err)
	}
}

func TestWriteMarkerErrorsAreWrapped(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := WriteMarker(filepath.Join(blocker, "sub", "marker"), "abc")
	if err == nil || !strings.HasPrefix(err.Error(), "write marker: ") {
		t.Errorf("WriteMarker under a file = %v", err)
	}
	// A directory where the marker file goes cannot be renamed over.
	dir := t.TempDir()
	m := filepath.Join(dir, "marker")
	if err := os.MkdirAll(filepath.Join(m, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteMarker(m, "abc"); err == nil {
		t.Error("WriteMarker over a non-empty directory succeeded")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".marker-*")); len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}
}

// A linked worktree's .git is a file; busy then asks git where the state files are.
func TestBusyInALinkedWorktreeAsksGit(t *testing.T) {
	f := newFixture(t)
	f.commit(map[string]string{mem(appHash, "a.md"): "1\n"})
	wt := filepath.Join(t.TempDir(), "wt")
	if _, err := f.repo.Run("worktree", "add", "-q", "--detach", wt); err != nil {
		t.Fatal(err)
	}
	linked := *f.repo
	linked.Dir = wt
	if plainGitDir(wt) != "" {
		t.Fatal("a linked worktree has a .git directory")
	}
	if busy(&linked) {
		t.Fatal("a quiet linked worktree reported busy")
	}
	gd, err := linked.Run("rev-parse", "--git-dir")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(strings.TrimSpace(gd), "MERGE_HEAD"), []byte(f.head()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !busy(&linked) {
		t.Error("a linked worktree stopped mid-merge was not busy")
	}
	// And a plain clone, same signal.
	if busy(f.repo) {
		t.Error("quiet plain clone reported busy")
	}
	if err := os.WriteFile(filepath.Join(f.repo.Dir, ".git", "CHERRY_PICK_HEAD"), []byte(f.head()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !busy(f.repo) {
		t.Error("plain clone mid cherry-pick not busy")
	}
}

func TestReadHeadSHARefusesUnsafeAndUnknownShapes(t *testing.T) {
	d := t.TempDir()
	gd := filepath.Join(d, ".git")
	if err := os.MkdirAll(filepath.Join(gd, "refs", "heads"), 0o700); err != nil {
		t.Fatal(err)
	}
	set := func(name, body string) {
		if err := os.WriteFile(filepath.Join(gd, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sha := strings.Repeat("ab", 20)
	set("HEAD", "ref: refs/heads/../../x\n")
	if got := readHeadSHA(d); got != "" {
		t.Errorf("a ref with .. read as %q", got)
	}
	set("HEAD", "ref: heads/main\n")
	if got := readHeadSHA(d); got != "" {
		t.Errorf("a ref outside refs/ read as %q", got)
	}
	set("HEAD", "ref: refs/heads/main\n")
	if got := readHeadSHA(d); got != "" {
		t.Errorf("a missing ref read as %q", got)
	}
	set(filepath.Join("refs", "heads", "main"), "not-a-sha\n")
	if got := readHeadSHA(d); got != "" {
		t.Errorf("a garbage ref file read as %q", got)
	}
	set(filepath.Join("refs", "heads", "main"), sha+"\n")
	if got := readHeadSHA(d); got != sha {
		t.Errorf("loose ref = %q, want %q", got, sha)
	}
	set("HEAD", sha+"\n")
	if got := readHeadSHA(d); got != sha {
		t.Errorf("detached = %q", got)
	}
	set("HEAD", "garbage\n")
	if got := readHeadSHA(d); got != "" {
		t.Errorf("garbage HEAD read as %q", got)
	}
	if got := readHeadSHA(t.TempDir()); got != "" {
		t.Errorf("a non-repo read as %q", got)
	}
}
