package changes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func firstUseOpts(f *fixture, marker, upper string) Options {
	o := f.opts("")
	o.Marker, o.Upper = marker, upper
	o.UpdateMarker, o.DeferMarker = true, true
	return o
}

// The first look of a session only sets the baseline: nothing is listed, and
// a plain clone needs no git process to know it.
func TestCompute_FirstUseBaselineNoGit(t *testing.T) {
	f := newFixture(t)
	f.commit(map[string]string{mem(appHash, "a.md"): "a"})
	head := f.head()
	marker := filepath.Join(t.TempDir(), "m")
	var res Result
	var err error
	n := spawns(func() { res, err = Compute(firstUseOpts(f, marker, "")) })
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("first use started %d git processes, want 0", n)
	}
	if res.Head != head || res.Since != head || res.Total != 0 || res.Reset || res.Busy || res.Changes == nil {
		t.Errorf("result %+v, want a baseline at %s", res, head)
	}
	if n := spawns(func() {
		if err := SaveMarker(f.repo, marker, res); err != nil {
			t.Fatal(err)
		}
	}); n != 0 {
		t.Errorf("saving the baseline started %d git processes", n)
	}
	if b, _ := os.ReadFile(marker); strings.TrimSpace(string(b)) != head {
		t.Errorf("marker %q, want %s", b, head)
	}
}

func TestCompute_FirstUseBaselineWithSyncedHeadAtHead(t *testing.T) {
	f := newFixture(t)
	f.commit(map[string]string{mem(appHash, "a.md"): "a"})
	head := f.head()
	var res Result
	n := spawns(func() { res, _ = Compute(firstUseOpts(f, filepath.Join(t.TempDir(), "m"), head)) })
	if n != 0 || res.Since != head || res.Head != head {
		t.Errorf("%d spawns, result %+v: want 0 and a baseline at %s", n, res, head)
	}
}

// The baseline is the synced head when that is behind HEAD: a note in HEAD is
// not on disk yet, and a baseline at HEAD would lose it.
func TestCompute_FirstUseBaselineIsUpperNotHead(t *testing.T) {
	f := newFixture(t)
	up := f.commit(map[string]string{mem(appHash, "a.md"): "a"})
	f.commit(map[string]string{mem(appHash, "b.md"): "b"})
	marker := filepath.Join(t.TempDir(), "m")
	var res Result
	n := spawns(func() { res, _ = Compute(firstUseOpts(f, marker, up)) })
	if n > 1 {
		t.Errorf("%d spawns, want at most 1", n)
	}
	if res.Head != up || res.Since != up {
		t.Errorf("head %s since %s, want the synced head %s", res.Head, res.Since, up)
	}
}

// A repo caught mid-rebase publishes nothing: no baseline, Busy set.
func TestCompute_FirstUseBusyRepoIsNotABaseline(t *testing.T) {
	f := newFixture(t)
	f.commit(map[string]string{mem(appHash, "a.md"): "a"})
	if err := os.MkdirAll(filepath.Join(f.repo.Dir, ".git", "rebase-merge"), 0o700); err != nil {
		t.Fatal(err)
	}
	res, err := Compute(firstUseOpts(f, filepath.Join(t.TempDir(), "m"), ""))
	if err != nil || !res.Busy {
		t.Errorf("busy=%v err=%v, want Busy", res.Busy, err)
	}
}

// A marker that names a commit that is gone is not covered: it takes the slow
// path, which reports the reset.
func TestCompute_FirstUseLostCommitStillResets(t *testing.T) {
	f := newFixture(t)
	f.commit(map[string]string{mem(appHash, "a.md"): "a"})
	marker := filepath.Join(t.TempDir(), "m")
	if err := WriteMarker(marker, strings.Repeat("a", 40)); err != nil {
		t.Fatal(err)
	}
	res, err := Compute(firstUseOpts(f, marker, ""))
	if err != nil || !res.Reset {
		t.Errorf("reset=%v err=%v, want Reset", res.Reset, err)
	}
}

func TestSaveMarkerNoGit(t *testing.T) {
	sha := func(c string) string { return strings.Repeat(c, 40) }
	write := func(t *testing.T, content string) string {
		p := filepath.Join(t.TempDir(), "m")
		if content != "" {
			if err := os.WriteFile(p, []byte(content+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return p
	}
	read := func(p string) string { b, _ := os.ReadFile(p); return strings.TrimSpace(string(b)) }
	cases := []struct {
		name   string
		marker string // content before ("" : no file)
		res    Result
		want   string
		moved  bool
	}{
		{"baseline creates", "", Result{Head: sha("b"), baseline: true}, sha("b"), true},
		{"baseline resets", sha("a"), Result{Head: sha("b"), baseline: true}, sha("b"), true},
		{"moved back writes", sha("c"), Result{Head: sha("b"), Since: sha("d"), MovedBack: true}, sha("b"), true},
		{"busy result is left", sha("a"), Result{Head: sha("b"), Busy: true}, sha("a"), false},
		{"marker still names Since", sha("a"), Result{Head: sha("b"), Since: sha("a")}, sha("b"), true},
		{"marker moved by another hook is left", sha("c"), Result{Head: sha("b"), Since: sha("a")}, sha("c"), false},
		{"no marker and no baseline is left", "", Result{Head: sha("b"), Since: sha("a")}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := write(t, tc.marker)
			var moved bool
			var err error
			n := spawns(func() { moved, err = SaveMarkerNoGit(p, tc.res) })
			if err != nil || moved != tc.moved || read(p) != tc.want || n != 0 {
				t.Errorf("moved=%v err=%v marker=%q spawns=%d; want moved=%v marker=%q 0 spawns", moved, err, read(p), n, tc.moved, tc.want)
			}
		})
	}
}

// An empty marker (made with touch) is reset like a missing one, and says so.
func TestCompute_FirstUseEmptyMarkerIsReset(t *testing.T) {
	f := newFixture(t)
	f.commit(map[string]string{mem(appHash, "a.md"): "a"})
	marker := filepath.Join(t.TempDir(), "m")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Compute(firstUseOpts(f, marker, ""))
	if err != nil || !res.Reset || res.Since != f.head() {
		t.Errorf("reset=%v since=%s err=%v, want a reset baseline at HEAD", res.Reset, res.Since, err)
	}
}

// A marker that already holds the commit is not rewritten (no temp file, no
// fsync): its inode stays and only its modification time moves, which is what
// keeps it from being pruned.
func TestWriteMarker_UnchangedIsRefreshedInPlace(t *testing.T) {
	p := filepath.Join(t.TempDir(), "m")
	sha := strings.Repeat("a", 40)
	if err := WriteMarker(p, sha); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(p)
	if err := WriteMarker(p, sha); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(p)
	if !os.SameFile(before, after) {
		t.Error("an unchanged marker was replaced")
	}
	if time.Since(after.ModTime()) > time.Minute {
		t.Errorf("modification time %v was not refreshed", after.ModTime())
	}
	other := strings.Repeat("b", 40)
	if err := WriteMarker(p, other); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); strings.TrimSpace(string(b)) != other {
		t.Errorf("marker %q, want %s", b, other)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
}
