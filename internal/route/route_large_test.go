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
		big := hugeDir(t, n, "my@app", "plain-name", "dot.name")
		restore := count(big)
		entries = 0
		if got := Candidates(escape(filepath.Join(big, "my@app"))); len(got) != 0 {
			t.Errorf("%d entries: resolved %v, want unresolved", n, got)
		}
		// Plain names and the usual separators resolve by probing, without a listing.
		for _, name := range []string{"plain-name", "dot.name"} {
			want := filepath.Join(big, name)
			if got := Candidates(escape(want)); len(got) != 1 || got[0] != want {
				t.Errorf("%d entries: %s -> %v", n, name, got)
			}
		}
		restore()
		costs = append(costs, entries)
	}
	if costs[0] != costs[1] || costs[0] > 3*(maxList+1) {
		t.Errorf("entries read of the huge dir: %v, want equal and at most %d each", costs, 3*(maxList+1))
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
