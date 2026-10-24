package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func brokenDirs(f *pullFixture) []string {
	m, _ := filepath.Glob(filepath.Join(f.repo.Dir, manifestStateDir, "parked.broken-*"))
	return m
}

func (f *pullFixture) writeParkState(content string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(parkStatePath(f.repo)), 0700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(parkStatePath(f.repo), []byte(content), 0600); err != nil {
		f.t.Fatal(err)
	}
}

// A parked.json that cannot be parsed must not halt sync forever. It is moved
// aside with the saved bytes, logged once, and the cycle carries on.
func TestPark_CorruptStateIsMovedAsideAndSyncContinues(t *testing.T) {
	f := newPullFixture(t, map[string]string{"note.md": "alpha\n"})
	logs := captureLog(t)
	f.writeParkState(`{"preRev": "x", "paths": [`)
	saved := filepath.Join(parkBytesDir(f.repo), "projects", "p1", "memory", "kept.md")
	if err := os.MkdirAll(filepath.Dir(saved), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(saved, []byte("saved bytes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.editLocal("fresh.md", "fresh\n")

	for i := 0; i < 3; i++ {
		if err := f.loop.flush(f.repo, f.roots, true); err != nil {
			t.Fatalf("cycle %d: %v", i+1, err)
		}
	}
	if !f.originHas("projects/p1/memory/fresh.md") {
		t.Error("a clean new note never reached origin")
	}
	if parkedOnDisk(f) {
		t.Error("the broken parked.json is still in place")
	}
	b := brokenDirs(f)
	if len(b) != 1 {
		t.Fatalf("want one parked.broken-* dir, got %v", b)
	}
	if _, err := os.Stat(filepath.Join(b[0], parkedJSON)); err != nil {
		t.Errorf("parked.json not kept in %s: %v", filepath.Base(b[0]), err)
	}
	if got, _ := os.ReadFile(filepath.Join(b[0], parkedDir, "projects", "p1", "memory", "kept.md")); string(got) != "saved bytes\n" {
		t.Errorf("saved bytes not recoverable from the broken dir: %q", got)
	}
	if n := strings.Count(logs.String(), "parked.broken-"); n != 1 {
		t.Errorf("want one loud log line naming the broken dir, got %d:\n%s", n, logs)
	}
}

// A state file that parses but is not valid (an escaping path, an unknown
// kind) is treated the same way.
func TestPark_InvalidStateIsMovedAside(t *testing.T) {
	for name, content := range map[string]string{
		"escaping path": `{"preRev":"","paths":[{"path":"../x","kind":"file"}]}`,
		"unknown kind":  `{"preRev":"","paths":[{"path":"a.md","kind":"weird"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newPullFixture(t, map[string]string{"note.md": "alpha\n"})
			captureLog(t)
			f.writeParkState(content)
			if err := f.loop.flush(f.repo, f.roots, true); err != nil {
				t.Fatal(err)
			}
			if parkedOnDisk(f) || len(brokenDirs(f)) != 1 {
				t.Errorf("state not moved aside: exists=%v broken=%v", parkedOnDisk(f), brokenDirs(f))
			}
		})
	}
}

// A restore that fails for the same path on 3 consecutive cycles is set aside
// the same way, for that path only. The count is kept in the state file.
func TestPark_RepeatedRestoreFailureIsSetAside(t *testing.T) {
	f := newPullFixture(t, map[string]string{"note.md": "alpha\n"})
	logs := captureLog(t)
	f.writeWork("aaa/first.md", "first\n")
	f.writeWork("zzz/last.md", "last\n")
	if _, err := parkUncommitted(f.repo); err != nil {
		t.Fatal(err)
	}
	// Block the restore of aaa/first.md: its directory is now a file.
	if err := os.RemoveAll(filepath.Join(f.repo.Dir, "aaa")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.repo.Dir, "aaa"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	f.editLocal("fresh.md", "fresh\n")

	for i := 1; i <= 2; i++ {
		if err := f.loop.flush(f.repo, f.roots, true); err == nil {
			t.Fatalf("cycle %d: want the restore error", i)
		}
		st, err := loadPark(f.repo)
		if err != nil || st == nil || len(st.Paths) != 1 || st.Failures["aaa/first.md"] != i {
			t.Fatalf("cycle %d: state = %+v, %v; want one path with failure count %d", i, st, err, i)
		}
		if len(brokenDirs(f)) != 0 {
			t.Fatalf("cycle %d: set aside too early", i)
		}
	}
	if err := f.loop.flush(f.repo, f.roots, true); err != nil {
		t.Fatalf("cycle 3: %v", err)
	}
	if parkedOnDisk(f) {
		t.Error("the stuck path is still recorded")
	}
	b := brokenDirs(f)
	if len(b) != 1 {
		t.Fatalf("want one parked.broken-* dir, got %v", b)
	}
	if got, _ := os.ReadFile(filepath.Join(b[0], parkedDir, "aaa", "first.md")); string(got) != "first\n" {
		t.Errorf("the stuck path's bytes are not in the broken dir: %q", got)
	}
	if got, _ := f.workFile("zzz/last.md"); got != "last\n" {
		t.Errorf("the other path was not restored: %q", got)
	}
	if !f.originHas("projects/p1/memory/fresh.md") {
		t.Error("sync did not resume")
	}
	if !strings.Contains(logs.String(), "parked.broken-") {
		t.Errorf("want a log line naming the broken dir:\n%s", logs)
	}
}
