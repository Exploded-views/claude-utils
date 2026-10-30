package testenv

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/listlimit"
)

// fakeFS makes the entry counter and candidate list injectable: counts maps a
// directory to its entry count (default def), bad lists unreadable ones.
func fakeFS(t *testing.T, cands []string, counts map[string]int, bad ...string) {
	t.Helper()
	oc, on := tempCandidates, countEntries
	t.Cleanup(func() { tempCandidates, countEntries = oc, on })
	tempCandidates = func() []string { return cands }
	countEntries = func(dir string) (int, error) {
		for _, b := range bad {
			if dir == b {
				return 0, errors.New("permission denied")
			}
		}
		return counts[dir], nil
	}
}

func TestPickBaseSkipsABaseWithABigAncestor(t *testing.T) {
	fakeFS(t, []string{"/fakebig/t", "/fakeok/t"}, map[string]int{"/fakebig": listlimit.Max + 1})
	if got, ok := pickBase(tempCandidates()); !ok || got != "/fakeok/t" {
		t.Errorf("pickBase = %q, %v; want /fakeok/t", got, ok)
	}
}

func TestPickBaseLimitIsInclusive(t *testing.T) {
	fakeFS(t, []string{"/fakeedge/t", "/fakeok/t"}, map[string]int{"/fakeedge": listlimit.Max})
	if got, _ := pickBase(tempCandidates()); got != "/fakeedge/t" {
		t.Errorf("an ancestor with exactly Max entries is fine; pickBase = %q", got)
	}
	fakeFS(t, []string{"/fakeedge/t", "/fakeok/t"}, map[string]int{"/fakeedge": listlimit.Max + 1})
	if got, _ := pickBase(tempCandidates()); got != "/fakeok/t" {
		t.Errorf("an ancestor with Max+1 entries is not; pickBase = %q", got)
	}
}

func TestPickBaseChecksEveryAncestorUpToRoot(t *testing.T) {
	fakeFS(t, []string{"/a/b/c/d", "/fakeok/t"}, map[string]int{"/": listlimit.Max + 1})
	// root is an ancestor of both
	if got, ok := pickBase(tempCandidates()); ok {
		t.Errorf("root is too big for every base; pickBase = %q", got)
	}
	fakeFS(t, []string{"/a/b/c/d", "/fakeok/t"}, map[string]int{"/a": listlimit.Max + 1})
	if got, _ := pickBase(tempCandidates()); got != "/fakeok/t" {
		t.Errorf("a far ancestor must count; pickBase = %q", got)
	}
}

func TestPickBaseOrder(t *testing.T) {
	big := listlimit.Max + 1
	for _, tc := range []struct {
		name   string
		counts map[string]int
		want   string
	}{
		{"first wins", map[string]int{}, "/c1/t"},
		{"second when first bad", map[string]int{"/c1": big}, "/c2/t"},
		{"third when two bad", map[string]int{"/c1": big, "/c2": big}, "/c3/t"},
		{"first wins over a later good one only when good", map[string]int{"/c2": big}, "/c1/t"},
	} {
		fakeFS(t, []string{"/c1/t", "/c2/t", "/c3/t"}, tc.counts)
		if got, ok := pickBase(tempCandidates()); !ok || got != tc.want {
			t.Errorf("%s: pickBase = %q, %v; want %q", tc.name, got, ok, tc.want)
		}
	}
}

func TestPickBaseSkipsAnUnreadableAncestorAndAnyBusyBase(t *testing.T) {
	fakeFS(t, []string{"/fakeun/t", "/fakebusy", "/fakeok/t"}, map[string]int{"/fakebusy": 205}, "/fakeun")
	if got, ok := pickBase(tempCandidates()); !ok || got != "/fakeok/t" {
		t.Errorf("pickBase = %q, %v; want /fakeok/t", got, ok)
	}
}

func TestPickBaseNoneQualifies(t *testing.T) {
	big := listlimit.Max + 1
	fakeFS(t, []string{"/c1/t", "/c2/t"}, map[string]int{"/c1": big, "/c2": big})
	if got, ok := pickBase(tempCandidates()); ok {
		t.Errorf("pickBase = %q, want none", got)
	}
}

func TestBigAncestorNamesTheDirectoryAndCount(t *testing.T) {
	fakeFS(t, nil, map[string]int{"/fakebig": 261})
	dir, n, err := BigAncestor("/fakebig/x/y")
	if dir != "/fakebig" || n != 261 || err != nil {
		t.Errorf("BigAncestor = %q, %d, %v", dir, n, err)
	}
	fakeFS(t, nil, nil, "/fakeun")
	dir, _, err = BigAncestor("/fakeun/x")
	if dir != "/fakeun" || err == nil {
		t.Errorf("unreadable: BigAncestor = %q, %v", dir, err)
	}
	fakeFS(t, nil, nil)
	if dir, _, err = BigAncestor("/fakeok/x"); dir != "" || err != nil {
		t.Errorf("small: BigAncestor = %q, %v", dir, err)
	}
}

// rec is a skipper that records the skip.
type rec struct{ msg string }

func (r *rec) Helper()                  {}
func (r *rec) Skipf(f string, a ...any) { r.msg = fmt.Sprintf(f, a...) }

func fakeInvalidDirs(t *testing.T, dirs ...string) {
	t.Helper()
	o := invalidDirs
	t.Cleanup(func() { invalidDirs = o })
	invalidDirs = func() []string { return dirs }
}

func TestSkipIfInvalidReportsINVALIDForABigAncestor(t *testing.T) {
	fakeFS(t, nil, map[string]int{"/fakebig": 261})
	fakeInvalidDirs(t, "/fakebig/T")
	r := &rec{}
	SkipIfInvalid(r)
	if !strings.HasPrefix(r.msg, "INVALID: ") || !strings.Contains(r.msg, "/fakebig") || !strings.Contains(r.msg, "261") {
		t.Errorf("skip message = %q, want INVALID: naming /fakebig and 261", r.msg)
	}
}

func TestSkipIfInvalidReportsINVALIDForAnUnreadableAncestor(t *testing.T) {
	fakeFS(t, nil, nil, "/fakeun")
	fakeInvalidDirs(t, "/fakeun/T")
	r := &rec{}
	SkipIfInvalid(r)
	if !strings.HasPrefix(r.msg, "INVALID: ") || !strings.Contains(r.msg, "/fakeun") {
		t.Errorf("skip message = %q", r.msg)
	}
}

func TestSkipIfInvalidChecksEveryDirectory(t *testing.T) {
	fakeFS(t, nil, map[string]int{"/fakehome": 300})
	fakeInvalidDirs(t, "/fakeok/T", "/fakehome/h")
	r := &rec{}
	SkipIfInvalid(r)
	if !strings.Contains(r.msg, "/fakehome") {
		t.Errorf("skip message = %q, want the temp HOME's ancestor", r.msg)
	}
}

func TestSkipIfInvalidLeavesAGoodFixtureAlone(t *testing.T) {
	fakeFS(t, nil, map[string]int{"/fakeok": listlimit.Max})
	fakeInvalidDirs(t, "/fakeok/T")
	r := &rec{}
	SkipIfInvalid(r)
	if r.msg != "" {
		t.Errorf("skipped a good fixture: %q", r.msg)
	}
}

func TestSmallTempDirPicksTheNextGoodBase(t *testing.T) {
	good := t.TempDir()
	bad := "/fakebig/T"
	fakeFS(t, []string{bad, good}, map[string]int{"/fakebig": 300})
	t.Setenv("TMPDIR", bad)
	cleanup := SmallTempDir()
	got := os.Getenv("TMPDIR")
	if filepath.Dir(got) != good {
		t.Fatalf("TMPDIR = %q, want a fresh dir under %s", got, good)
	}
	cleanup()
	if _, err := os.Stat(got); !os.IsNotExist(err) {
		t.Errorf("cleanup left %s behind", got)
	}
}

func TestSmallTempDirLeavesTMPDIRWhenNoBaseQualifies(t *testing.T) {
	fakeFS(t, []string{"/c1/t", "/c2/t"}, map[string]int{"/c1": 300, "/c2": 300})
	t.Setenv("TMPDIR", "/c1/t")
	cleanup := SmallTempDir()
	defer cleanup()
	if got := os.Getenv("TMPDIR"); got != "/c1/t" {
		t.Errorf("TMPDIR = %q, want it unchanged", got)
	}
}

func TestDefaultCandidatesOrder(t *testing.T) {
	t.Setenv(EnvTmpCandidates, "")
	want := []string{os.TempDir(), "/tmp", "/var/tmp"}
	got := tempCandidates()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("tempCandidates = %v, want %v (current temp dir, /tmp, /var/tmp)", got, want)
	}
}

func TestCandidatesEnvOverride(t *testing.T) {
	t.Setenv(EnvTmpCandidates, "/x/one"+string(os.PathListSeparator)+"/y/two")
	if got := tempCandidates(); fmt.Sprint(got) != "[/x/one /y/two]" {
		t.Errorf("tempCandidates = %v, want the override list", got)
	}
}
