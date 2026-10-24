package sync

import "testing"

func TestFlushSkipsPushWhenNotAllowed(t *testing.T) {
	f := newPullFixture(t, map[string]string{"a.md": "one\n"})
	f.editLocal("b.md", "two\n")
	f.loop.PushAllowed = func() bool { return false }
	if err := f.loop.flush(f.repo, f.roots, true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.Run("show", "origin/main:projects/p1/memory/b.md"); err == nil {
		t.Fatal("must not push while the repo is not private")
	}
	if out, _ := f.repo.Run("log", "--oneline", "origin/main..HEAD"); out == "" {
		t.Fatal("local commit must still be made")
	}
	f.loop.PushAllowed = func() bool { return true }
	if err := f.loop.flush(f.repo, f.roots, true); err != nil {
		t.Fatal(err)
	}
	if got := f.originFile("b.md"); got != "two\n" && got != "two" {
		t.Fatalf("push should resume, origin has %q", got)
	}
}
