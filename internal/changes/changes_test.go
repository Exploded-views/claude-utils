package changes

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// fixture is a sync repo with helpers to commit memory files.
type fixture struct {
	t      *testing.T
	repo   *gitwt.Repo
	claude string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	repo := gitwt.New(t.TempDir())
	if err := repo.Init("main"); err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, repo: repo, claude: filepath.Join(t.TempDir(), "projects")}
}

// commit writes files (repo-relative path -> content; "" deletes) and commits.
// It returns the new HEAD.
func (f *fixture) commit(files map[string]string) string {
	f.t.Helper()
	for rel, content := range files {
		p := filepath.Join(f.repo.Dir, filepath.FromSlash(rel))
		if content == "" {
			if err := os.Remove(p); err != nil {
				f.t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0600); err != nil {
			f.t.Fatal(err)
		}
	}
	if err := f.repo.AddAll(); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.repo.Commit("auto: test"); err != nil {
		f.t.Fatal(err)
	}
	return f.head()
}

func (f *fixture) head() string {
	f.t.Helper()
	out, err := f.repo.Run("rev-parse", "HEAD")
	if err != nil {
		f.t.Fatal(err)
	}
	return strings.TrimSpace(out)
}

func (f *fixture) opts(since string) Options {
	return Options{SyncDir: f.repo.Dir, ClaudeDir: f.claude, Since: since, Limit: 20}
}

const (
	appHash   = "-home-alice-src-app"
	otherHash = "-home-alice-src-other"
)

func mem(hash, name string) string { return "projects/" + hash + "/memory/" + name }

func find(r Result, file string) *Change {
	for i := range r.Changes {
		if r.Changes[i].File == file {
			return &r.Changes[i]
		}
	}
	return nil
}

func TestCompute_AddedChangedDeleted(t *testing.T) {
	f := newFixture(t)
	base := f.commit(map[string]string{
		mem(appHash, "MEMORY.md"): "# one\n",
		mem(appHash, "gone.md"):   "bye\n",
		mem(appHash, "same.md"):   "same\n",
	})
	head := f.commit(map[string]string{
		mem(appHash, "MEMORY.md"): "# two\n",
		mem(appHash, "new.md"):    "new\n",
		mem(appHash, "gone.md"):   "",
	})

	r, err := Compute(f.opts(base))
	if err != nil {
		t.Fatal(err)
	}
	if r.Head != head || r.Since != base {
		t.Errorf("head/since = %s/%s, want %s/%s", r.Head, r.Since, head, base)
	}
	if r.Total != 3 || len(r.Changes) != 3 || r.More != 0 {
		t.Fatalf("total=%d len=%d more=%d: %+v", r.Total, len(r.Changes), r.More, r.Changes)
	}
	for file, want := range map[string]string{"MEMORY.md": "changed", "new.md": "added", "gone.md": "deleted"} {
		c := find(r, file)
		if c == nil || c.Status != want {
			t.Errorf("%s: got %+v, want status %s", file, c, want)
			continue
		}
		if c.Project != appHash || c.Path != mem(appHash, file) {
			t.Errorf("%s: project/path = %s / %s", file, c.Project, c.Path)
		}
		if c.Local != filepath.Join(f.claude, appHash, "memory", file) {
			t.Errorf("%s: local = %s", file, c.Local)
		}
	}
	if find(r, "same.md") != nil {
		t.Error("untouched file reported")
	}
}

func TestCompute_NothingNewAtHead(t *testing.T) {
	f := newFixture(t)
	head := f.commit(map[string]string{mem(appHash, "a.md"): "a\n"})
	r, err := Compute(f.opts(head))
	if err != nil {
		t.Fatal(err)
	}
	if r.Total != 0 || r.Changes == nil || len(r.Changes) != 0 {
		t.Errorf("want an empty, non-nil list, got %+v", r)
	}
}

func TestCompute_SinceAcceptsAnyRev(t *testing.T) {
	f := newFixture(t)
	base := f.commit(map[string]string{mem(appHash, "a.md"): "a\n"})
	f.commit(map[string]string{mem(appHash, "b.md"): "b\n"})
	for _, since := range []string{base, base[:8], "HEAD~1", "main~1"} {
		r, err := Compute(f.opts(since))
		if err != nil {
			t.Errorf("since %q: %v", since, err)
			continue
		}
		if r.Since != base || r.Total != 1 {
			t.Errorf("since %q: since=%s total=%d", since, r.Since, r.Total)
		}
	}
}

func TestCompute_SinceFromMarkerFile(t *testing.T) {
	f := newFixture(t)
	base := f.commit(map[string]string{mem(appHash, "a.md"): "a\n"})
	f.commit(map[string]string{mem(appHash, "b.md"): "b\n"})
	marker := filepath.Join(t.TempDir(), "seen")
	if err := os.WriteFile(marker, []byte(base+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	o := f.opts("")
	o.Marker = marker
	r, err := Compute(o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Since != base || r.Total != 1 || r.Changes[0].File != "b.md" {
		t.Errorf("got %+v", r)
	}
}

func TestCompute_BadSince(t *testing.T) {
	f := newFixture(t)
	f.commit(map[string]string{mem(appHash, "a.md"): "a\n"})
	junk := filepath.Join(t.TempDir(), "junk")
	if err := os.WriteFile(junk, []byte("not a revision\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// --since is a revision only: a marker path is not accepted.
	for _, since := range []string{"deadbeefdeadbeef", "no-such-branch", "HEAD~5", junk, "--output=/tmp/x", "-1"} {
		if _, err := Compute(f.opts(since)); err == nil {
			t.Errorf("since %q: want an error", since)
		}
	}
}

func TestCompute_BadMarker(t *testing.T) {
	f := newFixture(t)
	f.commit(map[string]string{mem(appHash, "a.md"): "a\n"})
	dir := t.TempDir()
	junk := filepath.Join(dir, "junk")
	gone := filepath.Join(dir, "gone")
	empty := filepath.Join(dir, "empty")
	for path, body := range map[string]string{junk: "--output=x\n", gone: strings.Repeat("ab", 20) + "\n", empty: ""} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Without UpdateMarker none of these may be silently repaired or created.
	for _, m := range []string{filepath.Join(dir, "missing"), junk, gone, empty} {
		before, _ := os.ReadFile(m)
		if _, err := Compute(Options{SyncDir: f.repo.Dir, ClaudeDir: f.claude, Marker: m, Limit: 20}); err == nil {
			t.Errorf("marker %q: want an error", m)
		}
		after, _ := os.ReadFile(m)
		if string(before) != string(after) {
			t.Errorf("marker %q was modified", m)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing marker was created without UpdateMarker")
	}
}

func TestCompute_NeedsExactlyOneOfSinceAndMarker(t *testing.T) {
	f := newFixture(t)
	f.commit(map[string]string{mem(appHash, "a.md"): "a\n"})
	marker := filepath.Join(t.TempDir(), "seen")
	cases := map[string]Options{
		"neither":             {},
		"both":                {Since: "HEAD", Marker: marker},
		"update with since":   {Since: "HEAD", UpdateMarker: true},
		"update with neither": {UpdateMarker: true},
	}
	for name, o := range cases {
		o.SyncDir, o.ClaudeDir = f.repo.Dir, f.claude
		if _, err := Compute(o); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("marker created despite the error")
	}
}

func TestCompute_EmptyRepoHasNoHead(t *testing.T) {
	f := newFixture(t)
	if _, err := Compute(f.opts("HEAD")); err == nil {
		t.Error("want an error for a repo with no commits")
	}
}

func TestCompute_OnlyMemoryFiles(t *testing.T) {
	f := newFixture(t)
	base := f.commit(map[string]string{mem(appHash, "a.md"): "a\n"})
	f.commit(map[string]string{
		".gitattributes":                    "MEMORY.md merge=x\n",
		"distilled/some-entry.md":           "d\n",
		"projects/" + appHash + "/stray.md": "not under memory/\n",
		mem(appHash, "b.md"):                "b\n",
	})
	r, err := Compute(f.opts(base))
	if err != nil {
		t.Fatal(err)
	}
	if r.Total != 1 || r.Changes[0].File != "b.md" {
		t.Errorf("got %+v", r.Changes)
	}
}

func TestCompute_ProjectFilter(t *testing.T) {
	f := newFixture(t)
	base := f.commit(map[string]string{mem(appHash, "a.md"): "a\n"})
	f.commit(map[string]string{
		mem(appHash, "b.md"):   "b\n",
		mem(otherHash, "c.md"): "c\n",
	})

	// By project directory: the escaped path is Claude's folder name.
	o := f.opts(base)
	o.Project = "/home/alice/src/app"
	r, err := Compute(o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Total != 1 || r.Changes[0].File != "b.md" {
		t.Errorf("dir filter: %+v", r.Changes)
	}

	// By Claude's folder name directly.
	o.Project = otherHash
	r, err = Compute(o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Total != 1 || r.Changes[0].File != "c.md" {
		t.Errorf("name filter: %+v", r.Changes)
	}

	// A project with no memory in the repo is not an error.
	o.Project = "/home/alice/src/unknown"
	r, err = Compute(o)
	if err != nil || r.Total != 0 {
		t.Errorf("unknown project: %+v %v", r, err)
	}
}

func TestCompute_CapsOutput(t *testing.T) {
	f := newFixture(t)
	base := f.commit(map[string]string{mem(appHash, "a.md"): "a\n"})
	files := map[string]string{}
	for _, n := range []string{"1", "2", "3", "4", "5"} {
		files[mem(appHash, "n"+n+".md")] = n + "\n"
	}
	f.commit(files)

	o := f.opts(base)
	o.Limit = 2
	r, err := Compute(o)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Changes) != 2 || r.Total != 5 || r.More != 3 {
		t.Fatalf("len=%d total=%d more=%d", len(r.Changes), r.Total, r.More)
	}
	if r.Changes[0].File != "n1.md" || r.Changes[1].File != "n2.md" {
		t.Errorf("cap should keep the first entries in path order: %+v", r.Changes)
	}
	if txt := r.Text(); !strings.Contains(txt, "...3 more") {
		t.Errorf("text should end with the overflow count:\n%s", txt)
	}

	o.Limit = 0 // unlimited
	r, err = Compute(o)
	if err != nil || len(r.Changes) != 5 || r.More != 0 {
		t.Errorf("unlimited: %d changes, more=%d, %v", len(r.Changes), r.More, err)
	}
}

func TestResult_Text(t *testing.T) {
	f := newFixture(t)
	base := f.commit(map[string]string{mem(appHash, "a.md"): "a\n"})
	f.commit(map[string]string{mem(appHash, "a.md"): "a2\n", mem(appHash, "b.md"): "b\n"})
	r, err := Compute(f.opts(base))
	if err != nil {
		t.Fatal(err)
	}
	txt := r.Text()
	for _, want := range []string{
		"2 memory files changed since " + base[:7],
		"changed  " + filepath.Join(f.claude, appHash, "memory", "a.md"),
		"added    " + filepath.Join(f.claude, appHash, "memory", "b.md"),
		"head " + r.Head[:7],
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("text missing %q:\n%s", want, txt)
		}
	}

	empty := Result{Head: r.Head, Since: r.Head, Changes: []Change{}}
	if txt := empty.Text(); !strings.Contains(txt, "no memory changes") {
		t.Errorf("empty text: %q", txt)
	}
}

func TestResult_JSONShape(t *testing.T) {
	f := newFixture(t)
	base := f.commit(map[string]string{mem(appHash, "a.md"): "a\n"})
	f.commit(map[string]string{mem(appHash, "b.md"): "b\n"})
	r, err := Compute(f.opts(base))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"head", "since", "total", "more", "busy", "reset", "changes"} {
		if _, ok := m[k]; !ok {
			t.Errorf("JSON missing %q: %s", k, b)
		}
	}
	c := m["changes"].([]any)[0].(map[string]any)
	for _, k := range []string{"status", "project", "file", "path", "local"} {
		if _, ok := c[k]; !ok {
			t.Errorf("change missing %q: %s", k, b)
		}
	}
}

func TestMarker(t *testing.T) {
	f := newFixture(t)
	head := f.commit(map[string]string{mem(appHash, "a.md"): "a\n"})
	marker := filepath.Join(t.TempDir(), "sub", "seen")

	// First use: no marker yet. Establish the baseline, report nothing.
	r, err := Compute(Options{SyncDir: f.repo.Dir, ClaudeDir: f.claude, Marker: marker, UpdateMarker: true, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if r.Total != 0 || r.Since != head {
		t.Errorf("baseline run: %+v", r)
	}
	if b, _ := os.ReadFile(marker); strings.TrimSpace(string(b)) != head {
		t.Errorf("marker = %q, want HEAD", b)
	}

	// New commit: reported once, then the marker moves up.
	f.commit(map[string]string{mem(appHash, "b.md"): "b\n"})
	o := Options{SyncDir: f.repo.Dir, ClaudeDir: f.claude, Marker: marker, UpdateMarker: true, Limit: 20}
	r, err = Compute(o)
	if err != nil || r.Total != 1 {
		t.Fatalf("second run: %+v %v", r, err)
	}
	r, err = Compute(o)
	if err != nil || r.Total != 0 {
		t.Fatalf("third run should see nothing new: %+v %v", r, err)
	}

	// Without UpdateMarker the marker file is left alone.
	f.commit(map[string]string{mem(appHash, "c.md"): "c\n"})
	before, _ := os.ReadFile(marker)
	o.UpdateMarker = false
	if _, err := Compute(o); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(marker)
	if string(before) != string(after) {
		t.Error("marker rewritten without UpdateMarker")
	}
}

// A revision that does not resolve must be an error, not a marker file that
// happens to be named like the revision.
func TestCompute_BadRevWithUpdateMarkerCreatesNothing(t *testing.T) {
	f := newFixture(t)
	f.commit(map[string]string{mem(appHash, "a.md"): "a\n"})
	dir := t.TempDir()
	wd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	o := f.opts("HEAD~5")
	o.UpdateMarker = true
	if _, err := Compute(o); err == nil {
		t.Error("want an error")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("stray files created: %v", entries)
	}
}

// The daemon pulls with --rebase, so a marker written at an unpushed local
// commit names a commit that is later rewritten. Compute diffs trees, so the
// marker keeps working while the old object still exists.
func TestMarker_SurvivesRebase(t *testing.T) {
	f := newFixture(t)
	base := f.commit(map[string]string{mem(appHash, "a.md"): "a\n"})
	local := f.commit(map[string]string{mem(appHash, "own.md"): "own\n"})
	marker := filepath.Join(t.TempDir(), "seen")
	if err := os.WriteFile(marker, []byte(local+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Simulate `pull --rebase`: the remote commit lands under the local one.
	if _, err := f.repo.Run("reset", "--hard", base); err != nil {
		t.Fatal(err)
	}
	f.commit(map[string]string{mem(appHash, "remote.md"): "r\n"})
	if _, err := f.repo.Run("cherry-pick", local); err != nil {
		t.Fatal(err)
	}
	r, err := Compute(Options{SyncDir: f.repo.Dir, ClaudeDir: f.claude, Marker: marker, UpdateMarker: true, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if r.Total != 1 || find(r, "remote.md") == nil {
		t.Fatalf("want only remote.md, got %+v", r)
	}
}

// A marker whose commit is gone (pruned after a rebase, or the repo was
// re-cloned) is reset to HEAD instead of leaving the hook silent for good.
func TestMarker_ResetWhenUnresolvable(t *testing.T) {
	f := newFixture(t)
	f.commit(map[string]string{mem(appHash, "a.md"): "a\n"})
	marker := filepath.Join(t.TempDir(), "seen")
	if err := os.WriteFile(marker, []byte(strings.Repeat("ab", 20)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	head := f.commit(map[string]string{mem(appHash, "new.md"): "n\n"})
	o := Options{SyncDir: f.repo.Dir, ClaudeDir: f.claude, Marker: marker, UpdateMarker: true, Limit: 20}
	r, err := Compute(o)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Reset || r.Total != 0 || len(r.Changes) != 0 || r.Since != head {
		t.Errorf("want a reset with nothing reported: %+v", r)
	}
	if b, _ := os.ReadFile(marker); strings.TrimSpace(string(b)) != head {
		t.Errorf("marker = %q, want HEAD", b)
	}
	// The next run is normal again.
	f.commit(map[string]string{mem(appHash, "next.md"): "x\n"})
	r, err = Compute(o)
	if err != nil || r.Reset || r.Total != 1 {
		t.Errorf("after reset: %+v %v", r, err)
	}
}

func TestMarker_WrittenAtomically(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new", "markers")
	path := filepath.Join(dir, "seen")
	if err := writeMarker(path, "abc"); err != nil {
		t.Fatal(err)
	}
	if err := writeMarker(path, "def"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "def\n" {
		t.Errorf("marker = %q", b)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0600 {
		t.Errorf("file mode = %v, want 0600", st.Mode().Perm())
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0700 {
		t.Errorf("dir mode = %v, want 0700", st.Mode().Perm())
	}
	// No temp files are left behind.
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("leftover files: %v", entries)
	}
}

// git sees a rename as a delete plus an add; --no-renames keeps it that way.
func TestCompute_RenameIsDeletePlusAdd(t *testing.T) {
	f := newFixture(t)
	body := strings.Repeat("the same long note body\n", 20)
	base := f.commit(map[string]string{mem(appHash, "old.md"): body})
	f.commit(map[string]string{mem(appHash, "old.md"): "", mem(appHash, "new.md"): body})
	r, err := Compute(f.opts(base))
	if err != nil {
		t.Fatal(err)
	}
	old, nu := find(r, "old.md"), find(r, "new.md")
	if r.Total != 2 || old == nil || old.Status != "deleted" || nu == nil || nu.Status != "added" {
		t.Errorf("want delete + add, got %+v", r.Changes)
	}
}

// Note contents must never leak into any output, only names and paths.
func TestOutput_NeverContainsNoteContent(t *testing.T) {
	const secret = "SECRET-NOTE-CONTENT-7f3a"
	f := newFixture(t)
	base := f.commit(map[string]string{mem(appHash, "a.md"): "a\n"})
	f.commit(map[string]string{mem(appHash, "a.md"): secret + " one\n", mem(appHash, "b.md"): secret + " two\n"})
	r, err := Compute(f.opts(base))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(r)
	if r.Total != 2 || strings.Contains(r.Text(), secret) || strings.Contains(string(b), secret) {
		t.Errorf("note content leaked (total %d):\n%s\n%s", r.Total, r.Text(), b)
	}
}

// While the daemon's pull --rebase has stopped on a conflict, HEAD points at a
// half-built history; report nothing and leave the marker alone.
func TestCompute_BusyDuringRebase(t *testing.T) {
	f := newFixture(t)
	base := f.commit(map[string]string{mem(appHash, "a.md"): "a\n"})
	marker := filepath.Join(t.TempDir(), "seen")
	if err := os.WriteFile(marker, []byte(base+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) error { _, err := f.repo.Run(args...); return err }
	if err := run("checkout", "-q", "-b", "side"); err != nil {
		t.Fatal(err)
	}
	f.commit(map[string]string{mem(appHash, "a.md"): "side\n"})
	if err := run("checkout", "-q", "main"); err != nil {
		t.Fatal(err)
	}
	f.commit(map[string]string{mem(appHash, "a.md"): "main\n"})
	if err := run("checkout", "-q", "side"); err != nil {
		t.Fatal(err)
	}
	if err := run("rebase", "main"); err == nil {
		t.Fatal("rebase should have stopped on a conflict")
	}

	o := Options{SyncDir: f.repo.Dir, ClaudeDir: f.claude, Marker: marker, UpdateMarker: true, Limit: 20}
	r, err := Compute(o)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Busy || r.Total != 0 || len(r.Changes) != 0 {
		t.Errorf("want an empty busy result: %+v", r)
	}
	if b, _ := os.ReadFile(marker); strings.TrimSpace(string(b)) != base {
		t.Errorf("marker moved to %q while busy", b)
	}
	if b, _ := json.Marshal(r); !strings.Contains(string(b), `"busy":true`) {
		t.Errorf("JSON should say busy: %s", b)
	}

	// Once the rebase is abandoned the repo is reported on again.
	if err := run("rebase", "--abort"); err != nil {
		t.Fatal(err)
	}
	if r, err := Compute(o); err != nil || r.Busy {
		t.Errorf("after abort: %+v %v", r, err)
	}
}

// A file name with a newline must not be able to forge extra lines in the
// text a hook feeds to the model.
func TestResult_TextQuotesUnprintableNames(t *testing.T) {
	f := newFixture(t)
	base := f.commit(map[string]string{mem(appHash, "a.md"): "a\n"})
	evil := "x.md\nchanged  forged.md\x1b[2J"
	f.commit(map[string]string{mem(appHash, evil): "e\n", mem(appHash, "ok.md"): "ok\n"})
	r, err := Compute(f.opts(base))
	if err != nil || r.Total != 2 {
		t.Fatalf("%+v %v", r, err)
	}
	txt := r.Text()
	if strings.Count(txt, "\n") != 3 { // header + two files
		t.Errorf("name injected extra lines:\n%q", txt)
	}
	if !strings.Contains(txt, strconv.Quote(filepath.Join(f.claude, appHash, "memory", evil))) {
		t.Errorf("unprintable name not quoted:\n%q", txt)
	}
	if !strings.Contains(txt, filepath.Join(f.claude, appHash, "memory", "ok.md")+"\n") {
		t.Errorf("plain name should stay unquoted:\n%q", txt)
	}
	// JSON is escaped by the encoder.
	if b, _ := json.Marshal(r); strings.Contains(string(b), "\n") || strings.Contains(string(b), "\x1b") {
		t.Errorf("raw control characters in JSON: %q", b)
	}
}
