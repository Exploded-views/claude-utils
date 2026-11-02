package sync

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func readHeldState(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, ".state", "held-back"))
	if err != nil {
		return nil
	}
	lines := strings.Fields(string(b))
	sort.Strings(lines)
	return lines
}

// The hook reads this file instead of asking git, so the commit that holds
// notes back is what records them (paths only, one per line).
func TestCommitMirror_RecordsHeldPaths(t *testing.T) {
	repo, cfg := commitFixture(t)
	writeRepoFile(t, repo, "denylist.txt", "nightjar\n")
	writeRepoFile(t, repo, "projects/p1/memory/ok.md", "fine\n")
	writeRepoFile(t, repo, "projects/p1/memory/body.md", "about Nightjar\n")
	writeRepoFile(t, repo, "projects/p1/memory/nightjar-name.md", "fine\n")

	if _, err := CommitMirror(repo, cfg, "auto: t"); err != nil {
		t.Fatal(err)
	}
	want := []string{"projects/p1/memory/body.md", "projects/p1/memory/nightjar-name.md"}
	if got := readHeldState(t, repo.Dir); !reflect.DeepEqual(got, want) {
		t.Fatalf("record = %v, want %v", got, want)
	}

	// Fixed: the next commit clears the record.
	writeRepoFile(t, repo, "projects/p1/memory/body.md", "rewritten\n")
	if err := os.Remove(filepath.Join(repo.Dir, "projects/p1/memory/nightjar-name.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := CommitMirror(repo, cfg, "auto: t"); err != nil {
		t.Fatal(err)
	}
	if got := readHeldState(t, repo.Dir); len(got) != 0 {
		t.Fatalf("record after the fix = %v, want none", got)
	}
}
