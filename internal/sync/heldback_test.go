package sync

import (
	"errors"
	"os"
	"reflect"
	"sort"
	"testing"
)

// heldFixture is a sync repo with one clean untracked note and two the
// denylist catches: one by its name, one by its content.
func heldFixtureNotes(t *testing.T) (paths []string, repoDir string) {
	t.Helper()
	repo, cfg := commitFixture(t)
	writeRepoFile(t, repo, "denylist.txt", "nightjar\n")
	writeRepoFile(t, repo, "projects/p1/memory/clean.md", "harmless\n")
	writeRepoFile(t, repo, "projects/p1/memory/nightjar-plan.md", "harmless\n")
	writeRepoFile(t, repo, "projects/p1/memory/body.md", "about Nightjar\n")
	got, err := HeldBack(repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return got, repo.Dir
}

func TestHeldBack_ListsPathAndContentHits(t *testing.T) {
	got, _ := heldFixtureNotes(t)
	sort.Strings(got)
	want := []string{"projects/p1/memory/body.md", "projects/p1/memory/nightjar-plan.md"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("held = %v, want %v", got, want)
	}
}

func TestHeldBack_ReadOnly(t *testing.T) {
	repo, cfg := commitFixture(t)
	writeRepoFile(t, repo, "denylist.txt", "nightjar\n")
	writeRepoFile(t, repo, "projects/p1/memory/body.md", "about Nightjar\n")
	if _, err := HeldBack(repo, cfg); err != nil {
		t.Fatal(err)
	}
	if staged, _ := repo.StagedChanges(); len(staged) != 0 {
		t.Errorf("HeldBack staged %v", staged)
	}
}

func TestHeldBack_ModifiedTrackedNoteCounts(t *testing.T) {
	repo, cfg := commitFixture(t)
	writeRepoFile(t, repo, "projects/p1/memory/a.md", "fine\n")
	if ok, err := CommitMirror(repo, cfg, "auto: t"); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	writeRepoFile(t, repo, "denylist.txt", "nightjar\n")
	writeRepoFile(t, repo, "projects/p1/memory/a.md", "now nightjar\n")
	got, err := HeldBack(repo, cfg)
	if err != nil || !reflect.DeepEqual(got, []string{"projects/p1/memory/a.md"}) {
		t.Fatalf("held = %v err=%v", got, err)
	}
}

func TestHeldBack_MissingDenylistStillAppliesSecretRules(t *testing.T) {
	repo, cfg := commitFixture(t)
	writeRepoFile(t, repo, "projects/p1/memory/leaky.md", "token "+fakeSecret()+"\n")
	writeRepoFile(t, repo, "projects/p1/memory/ok.md", "fine\n")
	got, err := HeldBack(repo, cfg)
	if err != nil || !reflect.DeepEqual(got, []string{"projects/p1/memory/leaky.md"}) {
		t.Fatalf("held = %v err=%v", got, err)
	}
}

func TestHeldBack_ScrubOffHoldsNothing(t *testing.T) {
	repo, cfg := commitFixture(t)
	cfg.Scrub = false
	writeRepoFile(t, repo, "projects/p1/memory/leaky.md", "token "+fakeSecret()+"\n")
	if got, err := HeldBack(repo, cfg); err != nil || len(got) != 0 {
		t.Fatalf("held = %v err=%v", got, err)
	}
}

// With the policy stopping every commit, "nothing is held" would be a lie.
func TestHeldBack_BlockedByTheDenylistPolicy(t *testing.T) {
	repo, cfg := commitFixture(t)
	writeRepoFile(t, repo, "projects/p1/memory/a.md", "fine\n")
	cfg.RequireDenylist = true
	if _, err := HeldBack(repo, cfg); !errors.Is(err, ErrDenylistBlocked) {
		t.Errorf("required and missing: err = %v", err)
	}
	cfg.RequireDenylist = false
	if err := os.Mkdir(cfg.DenylistPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := HeldBack(repo, cfg); !errors.Is(err, ErrDenylistBlocked) {
		t.Errorf("unreadable: err = %v", err)
	}
}
