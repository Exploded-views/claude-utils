package route

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// hugeDir makes a directory of n files with the named subdirectories in it.
func hugeDir(t *testing.T, n int, subs ...string) string {
	t.Helper()
	big := filepath.Join(t.TempDir(), "T")
	if err := os.MkdirAll(big, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := os.WriteFile(filepath.Join(big, fmt.Sprintf("tmp.%05d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range subs {
		if err := os.MkdirAll(filepath.Join(big, s), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return big
}

func TestUnusualCharInSmallDirResolvesByListing(t *testing.T) {
	big := hugeDir(t, 20, "my@app")
	want := filepath.Join(big, "my@app")
	if got := Candidates(escape(want)); len(got) != 1 || got[0] != want {
		t.Fatalf("small dir: %v, want [%s]", got, want)
	}
}

func TestUnusualCharInHugeDirIsUnresolvedAtFlatCost(t *testing.T) {
	var mu sync.Mutex
	var entries int
	count := func(big string) func() {
		return SetReadDirForTest(func(dir string, n int) ([]os.DirEntry, error) {
			es, err := ReadDirN(dir, n)
			if dir == big {
				mu.Lock()
				entries += len(es)
				mu.Unlock()
			}
			return es, err
		})
	}
	var costs []int
	for _, n := range []int{400, 2000} {
		big := hugeDir(t, n, "my@app", "plainname", "plain-name", "dot.name")
		restore := count(big)
		entries = 0
		if got := Candidates(escape(filepath.Join(big, "my@app"))); len(got) != 0 {
			t.Errorf("%d entries: resolved %v, want unresolved", n, got)
		}
		// A name with no '-' in it is exact and resolves by probing, without a
		// listing; one with a '-' could be an unlisted sibling's and does not.
		want := filepath.Join(big, "plainname")
		if got := Candidates(escape(want)); len(got) != 1 || got[0] != want {
			t.Errorf("%d entries: plainname -> %v", n, got)
		}
		for _, name := range []string{"plain-name", "dot.name"} {
			if got := Candidates(escape(filepath.Join(big, name))); len(got) != 0 {
				t.Errorf("%d entries: %s resolved to %v, want unresolved (ambiguous)", n, name, got)
			}
		}
		restore()
		costs = append(costs, entries)
	}
	if costs[0] != costs[1] || costs[0] > 4*(maxList+1) {
		t.Errorf("entries read of the huge dir: %v, want equal and at most %d each", costs, 4*(maxList+1))
	}
}

// A live project with an unusual character under a huge dir is reported once in
// the log and in the summary; an old folder under the same dir is not.
func TestLiveFolderUnderHugeDirIsReported(t *testing.T) {
	big := hugeDir(t, 400, "my@app", "gone@old")
	projects := t.TempDir()
	mk := func(dir string, age time.Duration) string {
		folder := escape(filepath.Join(big, dir))
		mem := filepath.Join(projects, folder, "memory")
		if err := os.MkdirAll(mem, 0o700); err != nil {
			t.Fatal(err)
		}
		f := filepath.Join(mem, "n.md")
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-age)
		for _, p := range []string{f, mem, filepath.Join(projects, folder)} {
			if err := os.Chtimes(p, old, old); err != nil {
				t.Fatal(err)
			}
		}
		return folder
	}
	live, old := mk("my@app", 24*time.Hour), mk("gone@old", 40*24*time.Hour)

	f := fileOf(Rule{Path: "/nowhere-at-all/**", Repo: "projects"})
	r := New(f, func(string) bool { return true })
	var logs []string
	r.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	r.ProjectsDir = projects
	for i := 0; i < 3; i++ {
		r.Decide(live)
		r.Decide(old)
		r.Refresh()
	}
	var large int
	for _, l := range logs {
		if strings.Contains(l, "too many entries") {
			large++
			if !strings.Contains(l, hash8(live)) || strings.Contains(l, hash8(old)) {
				t.Errorf("wrong folder in %q", l)
			}
		}
	}
	if large != 1 {
		t.Errorf("%d log lines about the large directory, want 1 (logs: %q)", large, logs)
	}
	s := r.Summarize([]string{live, old})
	if len(s.TooLarge) != 1 || s.TooLarge[0] != hash8(live) {
		t.Errorf("summary lists %v, want only %s", s.TooLarge, hash8(live))
	}
}

// The documented limit: under a directory of more than maxList entries a name
// the probe cannot spell (non-ASCII, '+', '@', or four or more separators in
// one component) stays unresolved, and a live one is reported as such.
func TestLiveUnspellableNamesUnderHugeDirAreReported(t *testing.T) {
	names := []string{"café-app", "a+b", "my-very-long-project-name"}
	big := hugeDir(t, 400, names...)
	projects := t.TempDir()
	r := New(fileOf(Rule{Path: "/nowhere-at-all/**", Repo: "projects"}), func(string) bool { return true })
	r.Logf = func(string, ...any) {}
	r.ProjectsDir = projects
	var folders []string
	for _, n := range names {
		folder := escape(filepath.Join(big, n))
		mem := filepath.Join(projects, folder, "memory")
		if err := os.MkdirAll(mem, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(mem, "n.md"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		folders = append(folders, folder)
	}
	s := r.Summarize(folders)
	got := map[string]bool{}
	for _, h := range s.TooLarge {
		got[h] = true
	}
	for i, f := range folders {
		if !got[hash8(f)] {
			t.Errorf("%s is live and unresolved under a huge dir but not listed (TooLarge %v)", names[i], s.TooLarge)
		}
	}
}

// The liveness check of a folder under a huge dir is file I/O: Warm runs it for
// many folders side by side, so it must not run under the router's lock one
// folder after another. Counted, not timed: at some moment two checks overlap.
func TestLargeLivenessChecksOverlapDuringWarm(t *testing.T) {
	r := New(fileOf(Rule{Remote: "github.com/me/*", Repo: "projects"}), func(string) bool { return true })
	r.Logf = func(string, ...any) {}
	var mu sync.Mutex
	cur, peak := 0, 0
	r.largeFn = func(string) bool {
		mu.Lock()
		cur++
		if cur > peak {
			peak = cur
		}
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		mu.Lock()
		cur--
		mu.Unlock()
		return true
	}
	var folders []string
	for i := 0; i < 64; i++ {
		folders = append(folders, fmt.Sprintf("-nonexistent-dir-%02d", i))
	}
	r.Warm(folders)
	if peak < 2 {
		t.Fatalf("at most %d liveness check(s) ran at once during Warm, want them side by side", peak)
	}
	// The decisions are unaffected.
	for _, f := range folders {
		if d := r.Decide(f); d.Reason != ReasonUnresolved {
			t.Fatalf("%s: %+v", f, d)
		}
	}
}

// Under a huge dir a name with a '-' in it fails closed: a sibling that was
// not listed (a@b next to a/b) cannot be ruled out, so it resolves to nothing
// rather than to the one the probe found. A small dir lists, and the two
// entries make the folder ambiguous there as well. This replaces the earlier
// expectation that the probe returns every spelling it can try.
func TestDashedNameUnderHugeDirIsAmbiguousNotAProbedSibling(t *testing.T) {
	gitEnv(t)
	build := func(filler int) string {
		big := hugeDir(t, filler, "a@b", "a")
		repo := filepath.Join(big, "a", "b")
		initRepo(t, repo, "git@github.com:me/the-wrong-one.git")
		return escape(repo)
	}
	decide := func(folder string) (Decision, []string) {
		r := New(fileOf(Rule{Remote: "github.com/me/*", Repo: "projects"}), func(string) bool { return true })
		r.Logf = func(string, ...any) {}
		return r.Decide(folder), r.Candidates(folder)
	}
	small, huge := build(10), build(400)
	ds, cs := decide(small)
	dh, ch := decide(huge)
	if len(cs) != 2 {
		t.Fatalf("small dir: %v, want both spellings", cs)
	}
	if len(ch) != 0 {
		t.Errorf("huge dir: candidates %v, want none (ambiguous)", ch)
	}
	if ds.Reason != ReasonUnresolved || dh != ds {
		t.Errorf("decision under a huge dir %+v, small dir %+v: want the same, unresolved", dh, ds)
	}
}

// ...and a live one is reported in status as ambiguous.
func TestLiveAmbiguousNameUnderHugeDirIsReportedAsAmbiguous(t *testing.T) {
	big := hugeDir(t, 400, "a")
	projects := t.TempDir()
	folder := escape(filepath.Join(big, "a", "b"))
	mem := filepath.Join(projects, folder, "memory")
	if err := os.MkdirAll(mem, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mem, "n.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := New(fileOf(Rule{Remote: "github.com/me/*", Repo: "projects"}), func(string) bool { return true })
	var logs []string
	r.Logf = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	r.ProjectsDir = projects
	s := r.Summarize([]string{folder})
	if len(s.TooLarge) != 1 {
		t.Fatalf("summary lists %v, want the folder", s.TooLarge)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "ambiguous, not synced") {
		t.Errorf("log does not say ambiguous, not synced: %q", logs)
	}
}

// A dashed name under a small dir still resolves by listing.
func TestDashedNameUnderSmallDirResolvesByListing(t *testing.T) {
	big := hugeDir(t, 20, "my-app")
	want := filepath.Join(big, "my-app")
	if got := Candidates(escape(want)); len(got) != 1 || got[0] != want {
		t.Fatalf("small dir: %v, want [%s]", got, want)
	}
}

// The gone folders under a huge dir cost no git process, dashed or not.
func TestGoneFoldersUnderHugeDirCostNoSpawn(t *testing.T) {
	big := hugeDir(t, 400)
	l := countSpawns(t)
	for _, n := range []string{"gone", "gone-1", "gone-1-sub", "x.y"} {
		if _, ok := remoteURL(filepath.Join(big, n)); ok {
			t.Fatalf("%s: remoteURL ok", n)
		}
		r := New(fileOf(Rule{Remote: "github.com/me/*", Repo: "projects"}), func(string) bool { return true })
		r.Decide(escape(filepath.Join(big, n)))
	}
	if n := l.n(); n != 0 {
		t.Errorf("%d git spawns for gone folders, want 0: %v", n, l.subs)
	}
}

// H2: when the branch through a too-large dir gives up on a '-', the WHOLE walk
// is unresolved, not just that branch. Otherwise a sibling that resolves
// through small listed dirs (T-b/c next to a huge T holding b/c) becomes the
// only candidate and routes the folder to the wrong repo.
func TestHugeDirGiveUpPoisonsTheWholeWalk(t *testing.T) {
	big := hugeDir(t, 400, "b/c")
	sibling := filepath.Join(filepath.Dir(big), "T-b", "c")
	if err := os.MkdirAll(sibling, 0o700); err != nil {
		t.Fatal(err)
	}
	folder := escape(filepath.Join(big, "b", "c"))
	if got := Candidates(folder); len(got) != 0 {
		t.Fatalf("candidates %v, want none (the sibling %s must not win alone)", got, sibling)
	}
	if _, large := candidates(folder, func(dir string) *listing { return readListing(dir) }); !large {
		t.Error("large = false, want true so the folder is reported")
	}
	// The rule that matches only the sibling must not claim the folder.
	f := fileOf(Rule{Path: filepath.Join(filepath.Dir(big), "T-b") + "/**", Repo: "projects"})
	r := New(f, func(string) bool { return true })
	r.Logf = func(string, ...any) {}
	if d := r.Decide(folder); d.Repo != "" || d.Reason != ReasonUnresolved {
		t.Errorf("decision %+v, want unresolved", d)
	}
	// and a live one is reported in status.
	projects := t.TempDir()
	mem := filepath.Join(projects, folder, "memory")
	if err := os.MkdirAll(mem, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mem, "n.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.ProjectsDir = projects
	if s := r.Summarize([]string{folder}); len(s.TooLarge) != 1 || s.TooLarge[0] != hash8(folder) {
		t.Errorf("summary lists %v, want %s", s.TooLarge, hash8(folder))
	}
}
