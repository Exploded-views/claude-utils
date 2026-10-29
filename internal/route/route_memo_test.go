package route

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
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

func newMemoTree(t *testing.T, nMissing int) *memoTree { return newMemoTreeN(t, nMissing, 2000) }

func newMemoTreeN(t *testing.T, nMissing, nFiller int) *memoTree {
	t.Helper()
	gitEnv(t)
	m := &memoTree{big: filepath.Join(t.TempDir(), "big"), reads: map[string]int{}}
	if err := os.MkdirAll(m.big, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < nFiller; i++ {
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
	readDir = func(dir string, n int) ([]os.DirEntry, error) {
		m.mu.Lock()
		m.reads[dir]++
		m.mu.Unlock()
		return ReadDirN(dir, n)
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
	readDir = func(dir string, n int) ([]os.DirEntry, error) {
		if dir == tmp {
			mu.Lock()
			reads++
			mu.Unlock()
		}
		return ReadDirN(dir, n)
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

// A directory that changes on every read (a busy $TMPDIR) must not make the
// walk start over for ever: it gives up after three walks, returns the answer
// it has, and does not remember it.
func TestCandidatesBoundedWhenDirKeepsChanging(t *testing.T) {
	m := newMemoTreeN(t, 0, 50) // small enough to be listed
	var mu sync.Mutex
	walks, bumps := 0, 0
	bumping := true
	inner := readDir
	readDir = func(dir string, n int) ([]os.DirEntry, error) {
		es, err := inner(dir, n)
		mu.Lock()
		defer mu.Unlock()
		// Bounded, so a missing cap fails the test instead of spinning.
		if dir == m.big && bumping && bumps < 200 {
			bumps++
			_ = os.WriteFile(filepath.Join(m.big, fmt.Sprintf("bump%03d", bumps)), nil, 0600)
		}
		return es, err
	}
	walkHook = func(string) {
		mu.Lock()
		walks++
		mu.Unlock()
	}
	r := memoRouter()
	want := filepath.Join(m.big, "repo0")
	got := r.Candidates(m.real[0])
	if len(got) != 1 || got[0] != want {
		t.Fatalf("answer %v, want [%s]", got, want)
	}
	if walks > 3 {
		t.Fatalf("%d walks while the dir kept changing, want at most 3", walks)
	}
	mu.Lock()
	bumping = false
	before := walks
	mu.Unlock()
	r.Candidates(m.real[0]) // not remembered: walks again, now on a quiet dir
	if walks <= before {
		t.Fatalf("answer from a changing dir was remembered: %d walks, was %d", walks, before)
	}
	// Quiet now, so it is remembered (a few tries: ancestors of the temp dir
	// may change under the test).
	for try := 0; try < 3; try++ {
		n := walks
		r.Candidates(m.real[0])
		if walks == n {
			return
		}
	}
	t.Fatal("stable answer not remembered")
}

func TestCandidatesFoldCaseOnCaseInsensitiveHosts(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("case-sensitive host")
	}
	dir := filepath.Join(t.TempDir(), "MixedCase")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	// Asked for in another case than the directory is spelled in.
	q := escape(strings.ToLower(dir))
	for name, got := range map[string][]string{"package": Candidates(q), "router": memoRouter().Candidates(q)} {
		if len(got) != 1 || got[0] != dir {
			t.Fatalf("%s: %v, want [%s]", name, got, dir)
		}
	}
}

// A folder whose directory did not exist when it was first asked about is
// found once the directory is made: the remembered answer is re-checked.
func TestMemoizedAnswerSeesDirCreatedLater(t *testing.T) {
	big := filepath.Join(t.TempDir(), "big")
	if err := os.MkdirAll(big, 0700); err != nil {
		t.Fatal(err)
	}
	r := memoRouter()
	dir := filepath.Join(big, "later")
	if got := r.Candidates(escape(dir)); len(got) != 0 {
		t.Fatalf("before: %v", got)
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if got := r.Candidates(escape(dir)); len(got) != 1 || got[0] != dir {
		t.Fatalf("after the dir was made: %v, want [%s]", got, dir)
	}
}

// Callers that ask for the same folder at once share one computation: the git
// calls behind a remote rule run once, not once per caller.
func TestConcurrentDecideOfOneFolderResolvesOnce(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	r := testRouter(t, fileOf(Rule{Remote: "github.com/me/*", Repo: "projects"}), []string{"projects"}, nil)
	r.Resolve = func(string) (string, resolution) {
		mu.Lock()
		calls++
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		return "github.com/me/a", resolutionOK
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d := r.Decide("-some-folder"); d.Repo != "projects" {
				t.Errorf("decision %+v", d)
			}
		}()
	}
	wg.Wait()
	if calls != 1 {
		t.Fatalf("Resolve ran %d times for 20 concurrent callers, want 1", calls)
	}
}

// The probe of a directory too big to list stats the query's spelling; on a
// case-folding host that succeeds for any case, so the candidate must still be
// spelled as the directory is on disk, found at flat cost (the big directory
// is read only up to maxList+1 entries, never in full). The directory is made
// here, so the answer does not depend on how big TMPDIR is.
func TestCandidatesFoldCaseUnderHugeDir(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("case-sensitive host")
	}
	// Its own root (not t.TempDir, which sits in TMPDIR) with a directory of
	// 20,000 entries, so the probe path is taken whatever TMPDIR holds.
	root, err := os.MkdirTemp("", "bigcase")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	big := filepath.Join(root, "BigDir")
	if err := os.MkdirAll(big, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20000; i++ {
		f, err := os.Create(filepath.Join(big, fmt.Sprintf("file%05d", i)))
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	dir := filepath.Join(big, "MixedCase", "SubDir")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	full, maxRead := 0, 0
	defer SetReadDirForTest(func(d string, n int) ([]os.DirEntry, error) {
		es, err := ReadDirN(d, n)
		if d == big {
			mu.Lock()
			if n <= 0 {
				full++
			}
			maxRead = max(maxRead, len(es))
			mu.Unlock()
		}
		return es, err
	})()
	q := escape(strings.ToLower(dir))
	for name, got := range map[string][]string{"package": Candidates(q), "router": memoRouter().Candidates(q)} {
		if len(got) != 1 || got[0] != dir {
			t.Errorf("%s: %v, want [%s]", name, got, dir)
		}
	}
	if full != 0 || maxRead > maxList+1 {
		t.Errorf("big dir listed: %d full reads, up to %d entries in one read (want 0 and at most %d)", full, maxRead, maxList+1)
	}
}
