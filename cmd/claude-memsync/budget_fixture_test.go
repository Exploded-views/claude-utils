//go:build budget

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/route"
	"github.com/MarimerLLC/claude-utils/internal/testenv"
)

// budgetTree is the owner's Mac in miniature: a $TMPDIR-shaped directory with
// many entries, about 600 project folders naming deleted directories under it,
// 300 real checkouts, and 100 other folders, for five repos.
type budgetTree struct {
	tmp     string   // the big $TMPDIR-shaped dir
	missing []string // folder names of deleted dirs under tmp
	real    []string // folder names of real checkouts under tmp
	other   []string // folder names of dirs outside tmp
}

// newBudgetTree builds the tree under root with nFiller entries in tmp. A
// checkout is a minimal .git (HEAD, config with an origin, empty objects and
// refs) written directly: git reads it, and 300 `git init`s would dominate the
// set-up time.
func newBudgetTree(tb testing.TB, root string, nFiller int) *budgetTree {
	tb.Helper()
	b := &budgetTree{tmp: filepath.Join(root, "private", "var", "folders", "ab", "cdef", "T")}
	if err := os.MkdirAll(b.tmp, 0o700); err != nil {
		tb.Fatal(err)
	}
	for i := 0; i < nFiller; i++ {
		if err := os.WriteFile(filepath.Join(b.tmp, fmt.Sprintf("tmp.%06d", i)), nil, 0o600); err != nil {
			tb.Fatal(err)
		}
	}
	for i := 0; i < 600; i++ {
		b.missing = append(b.missing, route.Escape(filepath.Join(b.tmp, fmt.Sprintf("gone%03d", i), "sub")))
	}
	for i := 0; i < 300; i++ {
		dir := filepath.Join(b.tmp, fmt.Sprintf("checkout%03d", i))
		writeMiniRepo(tb, dir, fmt.Sprintf("git@github.com:me/r%03d.git", i))
		b.real = append(b.real, route.Escape(dir))
	}
	for i := 0; i < 100; i++ {
		dir := filepath.Join(root, "other", fmt.Sprintf("o%03d", i))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			tb.Fatal(err)
		}
		b.other = append(b.other, route.Escape(dir))
	}
	return b
}

func writeMiniRepo(tb testing.TB, dir, origin string) {
	tb.Helper()
	g := filepath.Join(dir, ".git")
	for _, d := range []string{"objects", "refs/heads"} {
		if err := os.MkdirAll(filepath.Join(g, d), 0o700); err != nil {
			tb.Fatal(err)
		}
	}
	files := map[string]string{
		"HEAD":   "ref: refs/heads/main\n",
		"config": "[core]\n\trepositoryformatversion = 0\n[remote \"origin\"]\n\turl = " + origin + "\n",
	}
	for n, c := range files {
		if err := os.WriteFile(filepath.Join(g, n), []byte(c), 0o600); err != nil {
			tb.Fatal(err)
		}
	}
}

// budgetRoutes is the routing table of the fixture: a path rule that places
// nothing (so every folder needs its directory), then remote rules for five
// repos.
func budgetRoutes() []map[string]string {
	return []map[string]string{
		{"path": "/nowhere-at-all/**", "repo": "projects"},
		{"remote": "github.com/me/r0*", "repo": "projects"},
		{"remote": "github.com/me/r1*", "repo": "client-a"},
		{"remote": "github.com/me/r2*", "repo": "client-b"},
		{"remote": "github.com/me/*", "repo": "client-c"},
	}
}

var budgetRepos = []string{"personal", "projects", "client-a", "client-b", "client-c"}

// loadAvg1 is the 1-minute load average, or -1 when it cannot be read.
func loadAvg1() float64 {
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			if v, err := strconv.ParseFloat(f[0], 64); err == nil {
				return v
			}
		}
	}
	out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output() // "{ 1.2 1.3 1.4 }"
	if err != nil {
		return -1
	}
	if f := strings.Fields(strings.Trim(string(out), "{} \n")); len(f) > 0 {
		if v, err := strconv.ParseFloat(f[0], 64); err == nil {
			return v
		}
	}
	return -1
}

// maxLoad is the 1-minute load average above which a timing result means
// nothing.
const maxLoad = 8.0

// timingBudget is for a test that gates on wall time or CPU: it logs the load
// average at the start and the end, and when either is above maxLoad the test
// is skipped as INVALID instead of passing or failing. Call fail for a missed
// budget (not t.Errorf), and defer done.
func timingBudget(t *testing.T) (fail func(format string, args ...any), done func()) {
	t.Helper()
	testenv.SkipIfInvalid(t)
	start := loadAvg1()
	t.Logf("load average at start: %.2f", start)
	var fails []string
	fail = func(format string, args ...any) { fails = append(fails, fmt.Sprintf(format, args...)) }
	done = func() {
		end := loadAvg1()
		t.Logf("load average at end: %.2f", end)
		if start > maxLoad || end > maxLoad {
			t.Skipf("INVALID: load average %.2f at start, %.2f at end, above %.0f; missed budgets would be %q", start, end, maxLoad, fails)
		}
		for _, f := range fails {
			t.Error(f)
		}
	}
	return fail, done
}

// logLoad logs the load average at the start of a budget test and again at its
// end. Counted tests (B4-B8) are not gated on it.
func logLoad(t *testing.T) {
	t.Helper()
	testenv.SkipIfInvalid(t)
	t.Logf("load average at start: %.2f", loadAvg1())
	t.Cleanup(func() { t.Logf("load average at end: %.2f", loadAvg1()) })
}
