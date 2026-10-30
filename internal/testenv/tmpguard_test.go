package testenv

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

type endRec struct {
	rec
	failed bool
	logs   []string
}

func (e *endRec) Failed() bool            { return e.failed }
func (e *endRec) Logf(f string, a ...any) { e.logs = append(e.logs, fmt.Sprintf(f, a...)) }

func TestEndCheckSkipsAPassingGateWhoseAncestorGrew(t *testing.T) {
	fakeFS(t, nil, map[string]int{"/fakebig": 300})
	fakeInvalidDirs(t, "/fakebig/T")
	e := &endRec{}
	EndCheck(e, invalidDirs())
	if !strings.HasPrefix(e.msg, "INVALID: ") || !strings.Contains(e.msg, "300") {
		t.Errorf("EndCheck skip = %q, want INVALID naming 300", e.msg)
	}
}

func TestEndCheckLogsButDoesNotHideAFailure(t *testing.T) {
	fakeFS(t, nil, map[string]int{"/fakebig": 300})
	fakeInvalidDirs(t, "/fakebig/T")
	e := &endRec{failed: true}
	EndCheck(e, invalidDirs())
	if e.msg != "" || len(e.logs) != 1 || !strings.HasPrefix(e.logs[0], "INVALID: ") {
		t.Errorf("skip %q, logs %q; want no skip and one INVALID log", e.msg, e.logs)
	}
}

func TestEndCheckLeavesAGoodRunAlone(t *testing.T) {
	fakeFS(t, nil, nil)
	fakeInvalidDirs(t, "/fakeok/T")
	e := &endRec{}
	EndCheck(e, invalidDirs())
	if e.msg != "" || len(e.logs) != 0 {
		t.Errorf("skip %q logs %q on a good run", e.msg, e.logs)
	}
}

type errRec struct{ errs []string }

func (e *errRec) Helper()                   {}
func (e *errRec) Errorf(f string, a ...any) { e.errs = append(e.errs, fmt.Sprintf(f, a...)) }

func TestRequireReadsFailsOnZero(t *testing.T) {
	e := &errRec{}
	RequireReads(e, "B4 reads", 0)
	if len(e.errs) != 1 || !strings.Contains(e.errs[0], "B4 reads") {
		t.Errorf("errors = %q, want one naming the gate", e.errs)
	}
	e = &errRec{}
	RequireReads(e, "B4 reads", 1)
	if len(e.errs) != 0 {
		t.Errorf("errors = %q on 1 read", e.errs)
	}
}

func TestSmallTempDirSwapsWhenTheOverrideFirstEntryQualifies(t *testing.T) {
	good := t.TempDir()
	fakeFS(t, []string{good}, nil)
	t.Setenv("TMPDIR", "/fakebusy/T")
	cleanup := SmallTempDir()
	defer cleanup()
	if got := os.Getenv("TMPDIR"); filepath.Dir(got) != good {
		t.Errorf("TMPDIR = %q, want a fresh dir under %s", got, good)
	}
}

func TestCountEntriesStopsReadingPastTheLimit(t *testing.T) {
	d := t.TempDir()
	for i := 0; i < 300; i++ {
		if err := os.WriteFile(filepath.Join(d, "f"+strconv.Itoa(i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	n, err := countEntries(d)
	if err != nil || n != readCap {
		t.Errorf("countEntries = %d, %v; want %d (reads stop at the cap, which is above every limit)", n, err, readCap)
	}
	if readCap <= listlimit.Max || readCap <= smallBase {
		t.Errorf("readCap %d must exceed both limits", readCap)
	}
}

func TestEndCheckRechecksTheStartDirsNotTheCurrentTMPDIR(t *testing.T) {
	// A gate (B1) points TMPDIR at its own big fixture mid-run; the end check
	// must look at what was captured at the start.
	fakeFS(t, nil, map[string]int{"/fakefixture": 10000})
	fakeInvalidDirs(t, "/fakeok/T")
	start := SkipIfInvalid(&rec{})
	fakeInvalidDirs(t, "/fakefixture/T") // TMPDIR moved to the big fixture
	e := &endRec{}
	EndCheck(e, start)
	if e.msg != "" || len(e.logs) != 0 {
		t.Errorf("end check looked at the moved TMPDIR: skip %q logs %q", e.msg, e.logs)
	}
}

func TestCheckDirsSkipsAsINVALIDWhenAStartDirGrew(t *testing.T) {
	fakeFS(t, nil, map[string]int{"/fakegrew": 300})
	r := &rec{}
	CheckDirs(r, []string{"/fakegrew/T"})
	if !strings.HasPrefix(r.msg, "INVALID: ") || !strings.Contains(r.msg, "300") {
		t.Errorf("CheckDirs skip = %q", r.msg)
	}
}

func TestCheckDirsBeforeAFailingVerdictGivesINVALIDNotFAIL(t *testing.T) {
	// The gate order: validity first, then the verdict. A gate that would fail
	// (0 reads) is skipped before RequireReads runs.
	fakeFS(t, nil, map[string]int{"/fakegrew": 300})
	r := &rec{}
	CheckDirs(r, []string{"/fakegrew/T"})
	e := &errRec{}
	if r.msg == "" {
		RequireReads(e, "B4", 0)
	}
	if r.msg == "" || len(e.errs) != 0 {
		t.Errorf("skip %q, errors %q: want INVALID before any verdict", r.msg, e.errs)
	}
}

func TestJudgeSkipsBeforeTheVerdictWhenInvalid(t *testing.T) {
	fakeFS(t, nil, map[string]int{"/fakegrew": 300})
	r := &rec{}
	ran := false
	Judge(r, []string{"/fakegrew/T"}, func() { ran = true })
	if ran || !strings.HasPrefix(r.msg, "INVALID: ") {
		t.Errorf("verdict ran %v, skip %q; want INVALID and no verdict", ran, r.msg)
	}
}

func TestJudgeRunsTheVerdictWhenValid(t *testing.T) {
	fakeFS(t, nil, nil)
	r := &rec{}
	ran := false
	Judge(r, []string{"/fakeok/T"}, func() { ran = true })
	if !ran || r.msg != "" {
		t.Errorf("verdict ran %v, skip %q; want the verdict and no skip", ran, r.msg)
	}
}

func TestBigAncestorCountsTheDirectoryItself(t *testing.T) {
	fakeFS(t, nil, map[string]int{"/fakeself/T": 300})
	if dir, n, _ := BigAncestor("/fakeself/T"); dir != "/fakeself/T" || n != 300 {
		t.Errorf("BigAncestor = %q, %d; want the dir itself (B1's fixture is such a dir)", dir, n)
	}
}

func TestForcedInvalidMessageNamesTheSwitch(t *testing.T) {
	m := invalidMsg("/d", readCap, errForced)
	if !strings.HasPrefix(m, "INVALID: ") || !strings.Contains(m, "forced by "+EnvForceEndInvalid) || strings.Contains(m, "cannot be read") {
		t.Errorf("message = %q, want INVALID: forced by %s, not a read error", m, EnvForceEndInvalid)
	}
}
