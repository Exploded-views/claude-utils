package changes

// Regression tests from review: a revert after history moves back is still
// reported, odd file names break nothing, and a huge change set stays cheap.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// History moves back (C -> B) and forward again (B -> D). a.md was v2 at C
// (reported), is v1 at B and D. The session was told about v2 and is never
// told a.md went back to v1.
func TestReview_FollowBackThenForwardMissesRevert(t *testing.T) {
	f := newFixture(t)
	b := f.commit(map[string]string{mem(appHash, "a.md"): "v1\n"})
	c := f.commit(map[string]string{mem(appHash, "a.md"): "v2\n"})
	m := filepath.Join(t.TempDir(), "m")
	if err := WriteMarker(m, c); err != nil { // the session has seen up to C
		t.Fatal(err)
	}
	if _, err := f.repo.Run("reset", "--hard", b); err != nil {
		t.Fatal(err)
	}
	o := Options{SyncDir: f.repo.Dir, ClaudeDir: f.claude, Marker: m, UpdateMarker: true, Limit: 20}
	r1, err := Compute(o)
	if err != nil {
		t.Fatal(err)
	}
	f.commit(map[string]string{mem(appHash, "other.md"): "o\n"})
	r2, err := Compute(o)
	if err != nil {
		t.Fatal(err)
	}
	if find(r1, "a.md") == nil && find(r2, "a.md") == nil {
		t.Errorf("a.md went v2 -> v1 on disk and was never reported (run1 %v, run2 %v)", r1.Changes, r2.Changes)
	}
}

func TestReview_OddNames(t *testing.T) {
	f := newFixture(t)
	base := f.commit(map[string]string{mem(appHash, "seed.md"): "s\n"})
	names := []string{"my notes.md", "ünïcødé ✓.md", "tab\there.md", "new\nline.md", "secret new\nline.md"}
	files := map[string]string{}
	for _, n := range names {
		files[mem(appHash, n)] = "repo\n"
	}
	f.commit(files)
	for _, n := range names {
		p := filepath.Join(f.claude, appHash, "memory", n)
		os.MkdirAll(filepath.Dir(p), 0700)
		os.WriteFile(p, []byte("local differs\n"), 0600)
	}
	o := f.opts(base)
	o.CompareLocal = true
	o.Hide = func(c Change) bool { return strings.Contains(c.Path, "secret") }
	r, err := Compute(o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Hidden != 1 || r.Total != 4 {
		t.Fatalf("hidden %d total %d", r.Hidden, r.Total)
	}
	for _, c := range r.Changes {
		nl := strings.Contains(c.Path, "\n")
		if nl == c.LocalKept {
			t.Errorf("%q: LocalKept=%v", c.Path, c.LocalKept)
		}
		if strings.Contains(c.Describe(), "\n") {
			t.Errorf("raw newline in %q", c.Describe())
		}
	}
	if strings.Contains(r.Text(), "secret") {
		t.Errorf("hidden name in text: %s", r.Text())
	}
}

// 3000 changes, limit 20: only the listed ones are compared, and it is quick.
func TestReview_Huge(t *testing.T) {
	f := newFixture(t)
	base := f.commit(map[string]string{mem(appHash, "seed.md"): "s\n"})
	files := map[string]string{}
	for i := 0; i < 3000; i++ {
		files[mem(appHash, fmt.Sprintf("n%04d.md", i))] = "x\n"
	}
	f.commit(files)
	o := f.opts(base)
	o.CompareLocal = true
	o.Hide = func(c Change) bool { return strings.HasSuffix(c.File, "7.md") }
	start := time.Now()
	r, err := Compute(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("3000 changes: %v, hidden %d total %d more %d listed %d", time.Since(start), r.Hidden, r.Total, r.More, len(r.Changes))
	if r.Hidden != 300 || r.Total != 2700 || len(r.Changes) != 20 || r.More != 2680 {
		t.Errorf("counts: %+v", r)
	}
}
