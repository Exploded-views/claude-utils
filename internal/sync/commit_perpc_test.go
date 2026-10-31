package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

func perPCFiles(t *testing.T, repo *gitwt.Repo) {
	t.Helper()
	for rel, c := range map[string]string{
		"config.json": `{"scrub":false}`, "denylist.txt": "x\n", "daemon.pid": "1\n",
		".state/visibility": "private\n", "distilled/DISTILLED.md": "d\n", "Config.json": "{}",
	} {
		writeRepoFile(t, repo, rel, c)
	}
}

func assertNoPerPC(t *testing.T, repo *gitwt.Repo) {
	t.Helper()
	for p := range committedFiles(t, repo) {
		if gitwt.LocalOnlyPath(p) {
			t.Errorf("HEAD holds per-PC path %q", p)
		}
	}
}

// .gitignore is synced content, so a remote can drop the per-PC lines from it.
// Nothing per-PC is committed all the same, ignore file or not.
func TestCommitMirror_NeverCommitsPerPCFiles(t *testing.T) {
	for _, gi := range []string{"", "# nothing ignored\n", "!config.json\n!.state/\n"} {
		repo, cfg := commitFixture(t)
		cfg.Scrub = false
		writeRepoFile(t, repo, ".gitignore", gi)
		perPCFiles(t, repo)
		writeRepoFile(t, repo, "projects/p/memory/a.md", "note\n")
		if _, err := CommitMirror(repo, cfg, "sync"); err != nil {
			t.Fatalf("gitignore %q: %v", gi, err)
		}
		assertNoPerPC(t, repo)
		if !committedFiles(t, repo)["projects/p/memory/a.md"] {
			t.Errorf("gitignore %q: the note was not committed", gi)
		}
	}
}

// Even with the exclude entries gone and a per-PC path already staged, the
// commit unstages it.
func TestCommitMirror_UnstagesAPerPCPathAlreadyInTheIndex(t *testing.T) {
	repo, cfg := commitFixture(t)
	cfg.Scrub = false
	perPCFiles(t, repo)
	os.WriteFile(filepath.Join(repo.Dir, ".git", "info", "exclude"), nil, 0o600)
	if _, err := repo.Run("add", "-f", "config.json", ".state/visibility", "Config.json"); err != nil {
		t.Fatal(err)
	}
	writeRepoFile(t, repo, "projects/p/memory/a.md", "note\n")
	if _, err := CommitMirror(repo, cfg, "sync"); err != nil {
		t.Fatal(err)
	}
	assertNoPerPC(t, repo)
}

// The exclude file is written beside the repo's config, where a remote cannot
// reach it.
func TestCommitMirror_WritesPerPCNamesToInfoExclude(t *testing.T) {
	repo, cfg := commitFixture(t)
	cfg.Scrub = false
	writeRepoFile(t, repo, "projects/p/memory/a.md", "note\n")
	if _, err := CommitMirror(repo, cfg, "sync"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(repo.Dir, ".git", "info", "exclude"))
	for _, want := range []string{"/config.json", "/.state/", "/daemon.pid", "/denylist.txt"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("info/exclude lacks %q:\n%s", want, b)
		}
	}
}

// A symlink in the sync dir is never committed, whatever put it there.
func TestCommitMirror_NeverCommitsALink(t *testing.T) {
	repo, cfg := commitFixture(t)
	cfg.Scrub = false
	writeRepoFile(t, repo, "projects/p/memory/a.md", "note\n")
	if err := os.Symlink("/etc/hosts", filepath.Join(repo.Dir, "projects", "p", "memory", "l.md")); err != nil {
		t.Skip(err)
	}
	if _, err := CommitMirror(repo, cfg, "sync"); err != nil {
		t.Fatal(err)
	}
	files := committedFiles(t, repo)
	if files["projects/p/memory/l.md"] || !files["projects/p/memory/a.md"] {
		t.Errorf("committed: %v", files)
	}
}

// A per-PC file that is staged does not hold up the commit of a note, even when
// it holds something the scrub would flag.
func TestCommitMirror_AStagedPerPCFileDoesNotBlockTheCommit(t *testing.T) {
	repo, cfg := commitFixture(t)
	writeRepoFile(t, repo, "config.json", `{"token":"`+fakeSecret()+`"}`)
	os.WriteFile(filepath.Join(repo.Dir, ".git", "info", "exclude"), nil, 0o600)
	if _, err := repo.Run("add", "-f", "config.json"); err != nil {
		t.Fatal(err)
	}
	writeRepoFile(t, repo, "projects/p/memory/a.md", "note\n")
	if ok, err := CommitMirror(repo, cfg, "sync"); err != nil || !ok {
		t.Fatalf("committed=%v err=%v", ok, err)
	}
	files := committedFiles(t, repo)
	if files["config.json"] || !files["projects/p/memory/a.md"] {
		t.Errorf("committed: %v", files)
	}
}

// A per-PC file that an old version tracked is removed from the repo by the
// next commit: staged deletions are not blocked.
func TestCommitMirror_CommitsTheDeletionOfATrackedPerPCFile(t *testing.T) {
	repo, cfg := commitFixture(t)
	cfg.Scrub = false
	writeRepoFile(t, repo, "config.json", "{}")
	if _, err := repo.Run("add", "-f", "config.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Commit("legacy"); err != nil { //nolint: a plain commit in test setup
		t.Fatal(err)
	}
	os.Remove(filepath.Join(repo.Dir, "config.json"))
	writeRepoFile(t, repo, "projects/p/memory/a.md", "note\n")
	if _, err := CommitMirror(repo, cfg, "sync"); err != nil {
		t.Fatal(err)
	}
	if committedFiles(t, repo)["config.json"] {
		t.Error("the deletion of config.json was held back")
	}
}
