package route

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// memoTree is a directory with many entries, folder names that escape to
// missing paths beneath it, and two real checkouts, with the dir reads counted.
type memoTree struct {
	big     string
	missing []string
	real    []string // routed to projects, then client-a
	mu      sync.Mutex
	reads   map[string]int
}

func (m *memoTree) bigReads() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reads[m.big]
}

func (m *memoTree) resetReads() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reads = map[string]int{}
}

func newMemoTree(t *testing.T, nMissing int) *memoTree {
	t.Helper()
	gitEnv(t)
	m := &memoTree{big: filepath.Join(t.TempDir(), "big"), reads: map[string]int{}}
	if err := os.MkdirAll(m.big, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2000; i++ {
		if err := os.WriteFile(filepath.Join(m.big, fmt.Sprintf("file%04d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for i, remote := range []string{"git@github.com:me/a.git", "git@github.com:me/b.git"} {
		dir := filepath.Join(m.big, fmt.Sprintf("repo%d", i))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		gitIn(t, dir, "init", "-q")
		gitIn(t, dir, "remote", "add", "origin", remote)
		m.real = append(m.real, escape(dir))
	}
	for i := 0; i < nMissing; i++ {
		m.missing = append(m.missing, escape(filepath.Join(m.big, fmt.Sprintf("gone%03d", i))))
	}
	prev, prevHook := readDir, walkHook
	readDir = func(dir string) ([]os.DirEntry, error) {
		m.mu.Lock()
		m.reads[dir]++
		m.mu.Unlock()
		return os.ReadDir(dir)
	}
	t.Cleanup(func() { readDir, walkHook = prev, prevHook })
	return m
}

func memoRouter() *Router {
	f := fileOf(
		Rule{Path: "/nowhere-at-all/**", Repo: "personal"},
		Rule{Remote: "github.com/me/a", Repo: "projects"},
		Rule{Remote: "github.com/me/b", Repo: "client-a"},
	)
	r := New(f, func(string) bool { return true })
	r.Logf = func(string, ...any) {}
	r.SSHHost = func(h string) string { return h }
	return r
}

func TestCandidatesReadsEachDirOncePerGeneration(t *testing.T) {
	m := newMemoTree(t, 50)
	r := memoRouter()
	for _, f := range m.missing {
		if d := r.Decide(f); d.Repo != "" {
			t.Fatalf("missing folder routed: %+v", d)
		}
	}
	for i, want := range []string{"projects", "client-a"} {
		if d := r.Decide(m.real[i]); d.Repo != want {
			t.Fatalf("real folder %d: %+v, want %s", i, d, want)
		}
	}
	if n := m.bigReads(); n != 1 {
		t.Fatalf("big dir read %d times for 52 folders, want 1", n)
	}
}

func TestCacheInvalidatedOnReplaceAndRefresh(t *testing.T) {
	m := newMemoTree(t, 0)
	r := memoRouter()
	// Both checkouts exist up front and only their git config changes later,
	// so the big dir itself never changes: only a reload can drop its listing.
	var dirs []string
	for _, name := range []string{"Replace", "Refresh"} {
		dir := filepath.Join(m.big, "late-"+name)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		gitIn(t, dir, "init", "-q")
		dirs = append(dirs, dir)
	}
	for i, step := range []struct {
		name  string
		reset func()
	}{
		{"Replace", func() { r.Replace(r.cur()) }},
		{"Refresh", r.Refresh},
	} {
		folder := escape(dirs[i])
		if d := r.Decide(folder); d.Reason != ReasonUnresolved {
			t.Fatalf("%s: before the origin exists: %+v", step.name, d)
		}
		gitIn(t, dirs[i], "remote", "add", "origin", "git@github.com:me/a.git")
		if d := r.Decide(folder); d.Reason != ReasonUnresolved {
			t.Fatalf("%s: cached answer changed without a reload: %+v", step.name, d)
		}
		m.resetReads()
		step.reset()
		if d := r.Decide(folder); d.Repo != "projects" {
			t.Fatalf("%s: after reload: %+v", step.name, d)
		}
		if n := m.bigReads(); n != 1 {
			t.Fatalf("%s: big dir read %d times after reload, want 1", step.name, n)
		}
	}
}

// A folder created after the first look at its parent is seen by the very
// next decision, without a reload; the parent is re-read once.
func TestChangedDirIsReadAgainOnce(t *testing.T) {
	m := newMemoTree(t, 3)
	r := memoRouter()
	r.Decide(m.missing[0])
	r.Decide(m.missing[1])
	if n := m.bigReads(); n != 1 {
		t.Fatalf("big dir read %d times before the change, want 1", n)
	}
	dir := filepath.Join(m.big, "fresh")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "init", "-q")
	gitIn(t, dir, "remote", "add", "origin", "git@github.com:me/a.git")
	if d := r.Decide(escape(dir)); d.Repo != "projects" {
		t.Fatalf("new dir not seen: %+v", d)
	}
	r.Decide(m.missing[2])
	if n := m.bigReads(); n != 2 {
		t.Fatalf("big dir read %d times after the change, want 2", n)
	}
}

func TestConcurrentDecideSharesDirReads(t *testing.T) {
	m := newMemoTree(t, 50)
	r := memoRouter()
	start := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < 35; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for _, f := range m.missing {
				r.Decide(f)
			}
		}()
	}
	close(start)
	wg.Wait()
	if n := m.bigReads(); n != 1 {
		t.Fatalf("big dir read %d times by 35 concurrent loops, want 1", n)
	}
}

func TestDecideCallsCandidatesOncePerFolder(t *testing.T) {
	m := newMemoTree(t, 0)
	var mu sync.Mutex
	walks := map[string]int{}
	walkHook = func(folder string) {
		mu.Lock()
		walks[folder]++
		mu.Unlock()
	}
	folder := m.real[0]
	// A path rule, then a remote rule: both need the directory. An ancestor
	// directory changing under the test (the system temp dir is busy) makes a
	// walk legitimately run again, so an attempt is retried when that happens.
	var first, afterRefresh int
	for try := 0; try < 5; try++ {
		r := memoRouter()
		mu.Lock()
		walks[folder] = 0
		mu.Unlock()
		if d := r.Decide(folder); d.Repo != "projects" {
			t.Fatalf("decision: %+v", d)
		}
		r.Decide(folder)
		first = walks[folder]
		r.Refresh()
		r.Decide(folder)
		afterRefresh = walks[folder]
		if first == 1 && afterRefresh == 2 {
			return
		}
	}
	t.Fatalf("%d walks for one folder, %d after Refresh; want 1 and 2", first, afterRefresh)
}

func BenchmarkDecideManyMissingFolders(b *testing.B) {
	m := newMemoTreeB(b, 500)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := memoRouter()
		for _, f := range m.missing {
			r.Decide(f)
		}
	}
}

// newMemoTreeB is newMemoTree for a benchmark: no git, no read counting.
func newMemoTreeB(b *testing.B, nMissing int) *memoTree {
	b.Helper()
	m := &memoTree{big: filepath.Join(b.TempDir(), "big")}
	if err := os.MkdirAll(m.big, 0700); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 2000; i++ {
		if err := os.WriteFile(filepath.Join(m.big, fmt.Sprintf("file%04d", i)), nil, 0600); err != nil {
			b.Fatal(err)
		}
	}
	for i := 0; i < nMissing; i++ {
		m.missing = append(m.missing, escape(filepath.Join(m.big, fmt.Sprintf("gone%03d", i))))
	}
	return m
}

// The shape seen on a Mac: many folders for directories that were under a huge
// $TMPDIR and are gone.
func TestMissingTmpdirFolderOneReadPerGeneration(t *testing.T) {
	gitEnv(t)
	tmp := filepath.Join(t.TempDir(), "private", "var", "folders", "ab", "cdef", "T")
	if err := os.MkdirAll(tmp, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5000; i++ {
		if err := os.Mkdir(filepath.Join(tmp, fmt.Sprintf("tmp.%05d", i)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	reads := 0
	prev := readDir
	readDir = func(dir string) ([]os.DirEntry, error) {
		if dir == tmp {
			mu.Lock()
			reads++
			mu.Unlock()
		}
		return os.ReadDir(dir)
	}
	t.Cleanup(func() { readDir = prev })
	r := memoRouter()
	decideAll := func() {
		for i := 0; i < 20; i++ {
			if d := r.Decide(escape(filepath.Join(tmp, fmt.Sprintf("gone%02d", i), "sub"))); d.Repo != "" {
				t.Fatalf("missing folder routed: %+v", d)
			}
		}
	}
	decideAll()
	if reads != 1 {
		t.Fatalf("T read %d times for 20 folders, want 1", reads)
	}
	r.Replace(r.cur())
	decideAll()
	if reads != 2 {
		t.Fatalf("T read %d times after Replace, want 2", reads)
	}
}
