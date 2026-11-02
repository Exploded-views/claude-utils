package sync

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func readHeldRaw(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, ".state", "held-back"))
	if err != nil {
		return ""
	}
	return string(b)
}

func readHeldState(t *testing.T, dir string) []string {
	t.Helper()
	var lines []string
	for _, l := range strings.Split(readHeldRaw(t, dir), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	sort.Strings(lines)
	return lines
}

// The hook reads this file instead of asking git, so the commit that holds
// notes back is what records them. A path held because its name matches the
// denylist is the private term itself: only counted, never written.
func TestCommitMirror_RecordsHeldPaths(t *testing.T) {
	repo, cfg := commitFixture(t)
	writeRepoFile(t, repo, "denylist.txt", "nightjar\n")
	writeRepoFile(t, repo, "projects/p1/memory/ok.md", "fine\n")
	writeRepoFile(t, repo, "projects/p1/memory/body.md", "about Nightjar\n")
	writeRepoFile(t, repo, "projects/p1/memory/nightjar-name.md", "fine\n")

	if _, err := CommitMirror(repo, cfg, "auto: t"); err != nil {
		t.Fatal(err)
	}
	want := []string{"#denylist", "#name-held 1", "projects/p1/memory/body.md"}
	if got := readHeldState(t, repo.Dir); !reflect.DeepEqual(got, want) {
		t.Fatalf("record = %v, want %v", got, want)
	}
	if strings.Contains(strings.ToLower(readHeldRaw(t, repo.Dir)), "nightjar") {
		t.Errorf("the record carries a denylisted name:\n%s", readHeldRaw(t, repo.Dir))
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

// When the denylist policy stops every commit, the old list must not stay
// behind: a denylist that is gone no longer hides names from the hook.
func TestCommitMirror_BlockedReplacesTheRecord(t *testing.T) {
	for _, how := range []string{"required and missing", "unreadable"} {
		t.Run(how, func(t *testing.T) {
			repo, cfg := commitFixture(t)
			writeRepoFile(t, repo, "denylist.txt", "nightjar\n")
			writeRepoFile(t, repo, "projects/p1/memory/body.md", "about Nightjar\n")
			writeRepoFile(t, repo, "projects/p1/memory/nightjar-name.md", "fine\n")
			if _, err := CommitMirror(repo, cfg, "auto: t"); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(cfg.DenylistPath()); err != nil {
				t.Fatal(err)
			}
			if how == "unreadable" {
				if err := os.Mkdir(cfg.DenylistPath(), 0o700); err != nil {
					t.Fatal(err)
				}
			} else {
				cfg.RequireDenylist = true
			}
			if _, err := CommitMirror(repo, cfg, "auto: t"); err != nil {
				t.Fatal(err)
			}
			if got := readHeldState(t, repo.Dir); !reflect.DeepEqual(got, []string{"#blocked"}) {
				t.Fatalf("record = %v, want only #blocked", got)
			}
		})
	}
}

// Scrub off means nothing is held, so an old record must not keep telling
// sessions their notes are not syncing.
func TestCommitMirror_ScrubOffClearsTheRecord(t *testing.T) {
	repo, cfg := commitFixture(t)
	writeRepoFile(t, repo, "projects/p1/memory/leaky.md", "token "+fakeSecret()+"\n")
	if _, err := CommitMirror(repo, cfg, "auto: t"); err != nil {
		t.Fatal(err)
	}
	if len(readHeldState(t, repo.Dir)) == 0 {
		t.Fatal("setup: no record")
	}
	cfg.Scrub = false
	if _, err := CommitMirror(repo, cfg, "auto: t"); err != nil {
		t.Fatal(err)
	}
	if got := readHeldState(t, repo.Dir); len(got) != 0 {
		t.Fatalf("record with scrub off = %v", got)
	}
}

// A note stays held for as long as it is wrong; the log (an unrotated file on
// macOS) names it when the held set changes, not every cycle.
func TestCommitMirror_LogsAHeldSetOnceUntilItChanges(t *testing.T) {
	repo, cfg := commitFixture(t)
	writeRepoFile(t, repo, "projects/p1/memory/leaky.md", "token "+fakeSecret()+"\n")
	logs := captureLog(t)
	for i := 0; i < 2; i++ {
		if _, err := CommitMirror(repo, cfg, "auto: t"); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(logs.String(), "holding back projects/p1/memory/leaky.md"); n != 1 {
		t.Fatalf("logged %d times over two cycles, want 1:\n%s", n, logs.String())
	}
	writeRepoFile(t, repo, "projects/p1/memory/second.md", "token "+fakeSecret()+"\n")
	if _, err := CommitMirror(repo, cfg, "auto: t"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "holding back projects/p1/memory/second.md") {
		t.Errorf("a changed set is not logged:\n%s", logs.String())
	}
}
