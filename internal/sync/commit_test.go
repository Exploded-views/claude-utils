package sync

import (
	"bytes"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// fakeSecret is assembled at run time so the source holds no credential-shaped
// literal.
func fakeSecret() string { return "ghp_" + strings.Repeat("a", 36) }

// commitFixture creates an initialized sync repo with one empty initial commit
// and a Config pointing at it.
func commitFixture(t *testing.T) (*gitwt.Repo, config.Config) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")

	dir := t.TempDir()
	repo := gitwt.New(dir)
	if err := repo.Init("main"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Run("commit", "--allow-empty", "-q", "-m", "root"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.SyncDir = dir
	return repo, cfg
}

func writeRepoFile(t *testing.T, repo *gitwt.Repo, rel, content string) {
	t.Helper()
	p := filepath.Join(repo.Dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// committedFiles lists the files in HEAD.
func committedFiles(t *testing.T, repo *gitwt.Repo) map[string]bool {
	t.Helper()
	out, err := repo.Run("ls-tree", "-r", "--name-only", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]bool{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l != "" {
			m[l] = true
		}
	}
	return m
}

// captureLog redirects the standard logger for the duration of the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

func TestCommitMirror_HoldsBackSecretsCommitsClean(t *testing.T) {
	repo, cfg := commitFixture(t)
	writeRepoFile(t, repo, "projects/p1/memory/clean.md", "just notes\n")
	writeRepoFile(t, repo, "projects/p1/memory/leaky.md", "token: "+fakeSecret()+"\n")

	logs := captureLog(t)
	committed, err := CommitMirror(repo, cfg, "auto: test")
	if err != nil {
		t.Fatal(err)
	}
	if !committed {
		t.Fatal("clean file should still be committed")
	}
	files := committedFiles(t, repo)
	if !files["projects/p1/memory/clean.md"] {
		t.Error("clean.md missing from commit")
	}
	if files["projects/p1/memory/leaky.md"] {
		t.Error("leaky.md must not be committed")
	}
	// Left on disk for the user to fix, and not staged.
	if _, err := os.Stat(filepath.Join(repo.Dir, "projects/p1/memory/leaky.md")); err != nil {
		t.Errorf("held-back file should stay in the work-tree: %v", err)
	}
	if out, _ := repo.Run("diff", "--cached", "--name-only"); strings.TrimSpace(out) != "" {
		t.Errorf("nothing should remain staged, got %q", out)
	}

	msg := logs.String()
	if !strings.Contains(msg, "leaky.md") || !strings.Contains(msg, "github-token") {
		t.Errorf("log should name file and rule, got %q", msg)
	}
	if strings.Contains(msg, fakeSecret()) || strings.Contains(msg, "ghp_") {
		t.Errorf("log must never contain matched text, got %q", msg)
	}
}

func TestCommitMirror_NothingCommittableReturnsFalse(t *testing.T) {
	repo, cfg := commitFixture(t)
	writeRepoFile(t, repo, "projects/p1/memory/leaky.md", "password = \""+strings.Repeat("x", 20)+"\"\n")
	captureLog(t)
	committed, err := CommitMirror(repo, cfg, "auto: test")
	if err != nil {
		t.Fatal(err)
	}
	if committed {
		t.Error("only held-back files: nothing should be committed")
	}
}

func TestCommitMirror_RetriesOnceFixed(t *testing.T) {
	repo, cfg := commitFixture(t)
	writeRepoFile(t, repo, "projects/p1/memory/note.md", "token: "+fakeSecret()+"\n")
	captureLog(t)
	if committed, _ := CommitMirror(repo, cfg, "auto: 1"); committed {
		t.Fatal("first cycle should hold the file")
	}
	writeRepoFile(t, repo, "projects/p1/memory/note.md", "token: (redacted)\n")
	committed, err := CommitMirror(repo, cfg, "auto: 2")
	if err != nil || !committed {
		t.Fatalf("fixed file should commit: committed=%v err=%v", committed, err)
	}
	if !committedFiles(t, repo)["projects/p1/memory/note.md"] {
		t.Error("note.md should be in HEAD after the fix")
	}
}

func TestCommitMirror_ModifiedFileKeepsPreviousVersion(t *testing.T) {
	repo, cfg := commitFixture(t)
	writeRepoFile(t, repo, "projects/p1/memory/note.md", "v1\n")
	captureLog(t)
	if _, err := CommitMirror(repo, cfg, "auto: 1"); err != nil {
		t.Fatal(err)
	}
	writeRepoFile(t, repo, "projects/p1/memory/note.md", "v2 "+fakeSecret()+"\n")
	writeRepoFile(t, repo, "projects/p1/memory/other.md", "other\n")
	if _, err := CommitMirror(repo, cfg, "auto: 2"); err != nil {
		t.Fatal(err)
	}
	got, err := repo.Run("show", "HEAD:projects/p1/memory/note.md")
	if err != nil {
		t.Fatal(err)
	}
	if got != "v1\n" {
		t.Errorf("HEAD note.md = %q, want the previous clean version", got)
	}
}

func TestCommitMirror_DeletesAreNotScanned(t *testing.T) {
	repo, cfg := commitFixture(t)
	writeRepoFile(t, repo, "projects/p1/memory/gone.md", "bye\n")
	captureLog(t)
	if _, err := CommitMirror(repo, cfg, "auto: 1"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repo.Dir, "projects/p1/memory/gone.md")); err != nil {
		t.Fatal(err)
	}
	committed, err := CommitMirror(repo, cfg, "auto: 2")
	if err != nil || !committed {
		t.Fatalf("delete should commit: committed=%v err=%v", committed, err)
	}
	if committedFiles(t, repo)["projects/p1/memory/gone.md"] {
		t.Error("deleted file still in HEAD")
	}
}

func TestCommitMirror_ScrubDisabled(t *testing.T) {
	repo, cfg := commitFixture(t)
	cfg.Scrub = false
	writeRepoFile(t, repo, "projects/p1/memory/leaky.md", "token: "+fakeSecret()+"\n")
	committed, err := CommitMirror(repo, cfg, "auto: test")
	if err != nil || !committed {
		t.Fatalf("scrub off should commit everything: committed=%v err=%v", committed, err)
	}
	if !committedFiles(t, repo)["projects/p1/memory/leaky.md"] {
		t.Error("leaky.md should be committed when scrub is off")
	}
}

func TestCommitMirror_Denylist(t *testing.T) {
	repo, cfg := commitFixture(t)
	writeRepoFile(t, repo, "denylist.txt", "# mine\nproject-nightjar\n")
	writeRepoFile(t, repo, "projects/p1/memory/a.md", "about Project-NightJar\n")
	writeRepoFile(t, repo, "projects/p1/memory/b.md", "harmless\n")

	logs := captureLog(t)
	committed, err := CommitMirror(repo, cfg, "auto: test")
	if err != nil || !committed {
		t.Fatalf("committed=%v err=%v", committed, err)
	}
	files := committedFiles(t, repo)
	if files["projects/p1/memory/a.md"] {
		t.Error("denylisted file committed")
	}
	if !files["projects/p1/memory/b.md"] {
		t.Error("clean file missing")
	}
	if files["denylist.txt"] {
		t.Error("denylist.txt must never be committed")
	}
	msg := logs.String()
	if !strings.Contains(msg, "a.md") || !strings.Contains(msg, "denylist:2") {
		t.Errorf("log should name file and denylist line, got %q", msg)
	}
	if strings.Contains(strings.ToLower(msg), "nightjar") {
		t.Errorf("log must not contain denylist pattern text, got %q", msg)
	}
}

func TestCommitMirror_DenylistNeverCommittedEvenWhenScrubOff(t *testing.T) {
	repo, cfg := commitFixture(t)
	cfg.Scrub = false
	writeRepoFile(t, repo, "denylist.txt", "anything\n")
	writeRepoFile(t, repo, "projects/p1/memory/a.md", "x\n")
	if _, err := CommitMirror(repo, cfg, "auto: test"); err != nil {
		t.Fatal(err)
	}
	if committedFiles(t, repo)["denylist.txt"] {
		t.Error("denylist.txt must never be committed")
	}
}

func TestCommitMirror_RequireDenylistMissingCommitsNothing(t *testing.T) {
	repo, cfg := commitFixture(t)
	cfg.RequireDenylist = true
	writeRepoFile(t, repo, "projects/p1/memory/a.md", "harmless\n")

	logs := captureLog(t)
	committed, err := CommitMirror(repo, cfg, "auto: test")
	if err != nil {
		t.Fatal(err)
	}
	if committed {
		t.Error("requireDenylist with no denylist must commit nothing")
	}
	if !strings.Contains(logs.String(), "denylist") {
		t.Errorf("log should say why, got %q", logs.String())
	}
	if out, _ := repo.Run("diff", "--cached", "--name-only"); strings.TrimSpace(out) != "" {
		t.Errorf("nothing should remain staged, got %q", out)
	}

	// Once the file exists the held-back work goes through.
	writeRepoFile(t, repo, "denylist.txt", "# empty on purpose\n")
	committed, err = CommitMirror(repo, cfg, "auto: test")
	if err != nil || !committed {
		t.Fatalf("committed=%v err=%v", committed, err)
	}
}

func TestCommitMirror_RequireDenylistIgnoredWhenScrubOff(t *testing.T) {
	repo, cfg := commitFixture(t)
	cfg.Scrub = false
	cfg.RequireDenylist = true
	writeRepoFile(t, repo, "projects/p1/memory/a.md", "harmless\n")
	committed, err := CommitMirror(repo, cfg, "auto: test")
	if err != nil || !committed {
		t.Fatalf("committed=%v err=%v", committed, err)
	}
}

func TestCommitMirror_InvalidDenylistFailsClosed(t *testing.T) {
	repo, cfg := commitFixture(t)
	writeRepoFile(t, repo, "denylist.txt", "(unclosed\n")
	writeRepoFile(t, repo, "projects/p1/memory/a.md", "harmless\n")

	logs := captureLog(t)
	committed, err := CommitMirror(repo, cfg, "auto: test")
	if err != nil {
		t.Fatal(err)
	}
	if committed {
		t.Error("an unreadable denylist must commit nothing")
	}
	if !strings.Contains(logs.String(), "line 1") {
		t.Errorf("log should point at the bad line, got %q", logs.String())
	}
}

func TestCommitMirror_UnbornBranch(t *testing.T) {
	// The first commit on a fresh `init` has no HEAD to diff against.
	repo, cfg := commitFixture(t)
	if _, err := repo.Run("update-ref", "-d", "HEAD"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Run("rev-parse", "--verify", "--quiet", "HEAD"); err == nil {
		t.Fatal("fixture should have an unborn HEAD")
	}
	writeRepoFile(t, repo, "denylist.txt", "x-never-matches\n")
	writeRepoFile(t, repo, "projects/p1/memory/clean.md", "ok\n")
	writeRepoFile(t, repo, "projects/p1/memory/leaky.md", "token: "+fakeSecret()+"\n")
	captureLog(t)
	committed, err := CommitMirror(repo, cfg, "init: test")
	if err != nil || !committed {
		t.Fatalf("committed=%v err=%v", committed, err)
	}
	files := committedFiles(t, repo)
	if !files["projects/p1/memory/clean.md"] || files["projects/p1/memory/leaky.md"] || files["denylist.txt"] {
		t.Errorf("unexpected commit contents: %v", files)
	}
}
