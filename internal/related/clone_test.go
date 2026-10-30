package related

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// bareWithNotes makes a local bare remote named <name>.git holding the files.
func bareWithNotes(t *testing.T, name string, files map[string]string) (bare string) {
	t.Helper()
	gitEnv(t)
	root := t.TempDir()
	bare = filepath.Join(root, name+".git")
	if _, err := gitwt.New(root).Run("init", "--bare", "-b", "main", bare); err != nil {
		t.Fatal(err)
	}
	// The fixtures must not leave background maintenance behind either: git
	// daemonizes it, and it would write into a TempDir after the test ended.
	for _, kv := range [][2]string{{"receive.autogc", "false"}, {"gc.auto", "0"}, {"maintenance.auto", "false"}} {
		if _, err := gitwt.New(root).Run("--git-dir="+bare, "config", kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	pushFiles(t, bare, files)
	return bare
}

// quietBare stops a bare repo from spawning gc or maintenance on receive.
func quietBare(t *testing.T, bare string) {
	t.Helper()
	for _, kv := range [][2]string{{"receive.autogc", "false"}, {"gc.auto", "0"}, {"maintenance.auto", "false"}} {
		if _, err := gitwt.New(filepath.Dir(bare)).Run("--git-dir="+bare, "config", kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
}

func pushFiles(t *testing.T, bare string, files map[string]string) {
	t.Helper()
	quietBare(t, bare)
	work := t.TempDir()
	w := gitwt.New(work)
	if err := w.Clone(bare); err != nil {
		t.Fatal(err)
	}
	w.Run("config", "maintenance.auto", "false")
	w.Run("config", "gc.auto", "0")
	w.Run("checkout", "-B", "main")
	for rel, body := range files {
		p := filepath.Join(work, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0700)
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	w.AddAll()
	if _, err := w.Commit("notes"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Run("push", "origin", "main"); err != nil {
		t.Fatal(err)
	}
}

func refs(t *testing.T, bare string) string {
	t.Helper()
	out, err := gitwt.New(bare).Run("for-each-ref")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestReadClone_FetchFollowsRemote(t *testing.T) {
	bare := bareWithNotes(t, "mem-project-b", map[string]string{"projects/p/memory/a.md": "one"})
	root := t.TempDir()
	rc, err := NewReadClone(root, "mem-project-b", bare)
	if err != nil {
		t.Fatal(err)
	}
	if rc.Exists() {
		t.Fatal("exists before fetch")
	}
	if err := rc.Fetch(); err != nil {
		t.Fatal(err)
	}
	before := rc.Head()
	if before == "" || !rc.Exists() {
		t.Fatal("no clone")
	}
	pushFiles(t, bare, map[string]string{"projects/p/memory/b.md": "two"})
	// A stray local edit in the exported tree is discarded.
	os.WriteFile(filepath.Join(rc.Dir(), "projects/p/memory/a.md"), []byte("edited"), 0600)
	if err := rc.Fetch(); err != nil {
		t.Fatal(err)
	}
	if rc.Head() == before {
		t.Fatal("did not follow the remote")
	}
	if b, _ := os.ReadFile(filepath.Join(rc.Dir(), "projects/p/memory/a.md")); string(b) != "one" {
		t.Fatalf("local edit survived: %q", b)
	}
	if _, err := os.Stat(filepath.Join(rc.Dir(), "projects/p/memory/b.md")); err != nil {
		t.Fatal("new file not exported")
	}
	if _, err := os.Stat(filepath.Join(rc.Dir(), ".git")); err == nil {
		t.Fatal("the readable tree must not be a git work-tree")
	}
	ch, ok := rc.ChangedSince(before)
	if !ok || len(ch) != 1 || ch[0].Status != "added" || ch[0].Path != "projects/p/memory/b.md" {
		t.Fatalf("ChangedSince = %+v %v", ch, ok)
	}
	if _, ok := rc.ChangedSince("deadbeef"); ok {
		t.Fatal("unknown commit reported as a baseline")
	}
	if _, stale := rc.Stale(); stale {
		t.Fatal("good fetch reported stale")
	}
}

// The read clone type has no way to write to the remote, and git refuses a
// push by hand even though the remote itself is writable.
func TestReadClone_NeverPushes(t *testing.T) {
	bare := bareWithNotes(t, "mem-project-b", map[string]string{"projects/p/memory/a.md": "one"})
	rc, _ := NewReadClone(t.TempDir(), "mem-project-b", bare)
	if err := rc.Fetch(); err != nil {
		t.Fatal(err)
	}
	typ := reflect.TypeOf(rc)
	for i := 0; i < typ.NumMethod(); i++ {
		n := strings.ToLower(typ.Method(i).Name)
		for _, bad := range []string{"push", "commit", "write", "run", "remote", "git"} {
			if strings.Contains(n, bad) {
				t.Errorf("ReadClone has method %s", typ.Method(i).Name)
			}
		}
	}
	before := refs(t, bare)
	g := gitwt.New(rc.store)
	if _, err := g.Run("push", "origin", refHead+":refs/heads/main"); err == nil {
		t.Fatal("a manual push reached the writable remote")
	}
	if refs(t, bare) != before {
		t.Fatal("the remote changed")
	}
}

func TestReadClone_BadNameAndNoURL(t *testing.T) {
	if _, err := NewReadClone(t.TempDir(), "../evil", "x"); err == nil {
		t.Fatal("path-like name accepted")
	}
	rc, _ := NewReadClone(t.TempDir(), "mem-project-b", "")
	if err := rc.Fetch(); err == nil {
		t.Fatal("fetch without URL")
	}
}

func TestURLRepoName(t *testing.T) {
	for in, want := range map[string]string{
		"git@alias-b:org/mem-project-b.git": "mem-project-b",
		"git@alias:mem-project-b":           "mem-project-b",
		"https://h/org/Mem-Project-B.git/":  "mem-project-b",
		"/srv/git/mem-project-b.git":        "mem-project-b",
		"ssh://git@h:22/org/x.git":          "x",
	} {
		if got := URLRepoName(in); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
}

// A relation to one repo can never fetch another: the URL names the repo.
func TestReadClone_URLMustNameTheRepo(t *testing.T) {
	bare := bareWithNotes(t, "mem-client-x", map[string]string{"a.md": "secret"})
	root := t.TempDir()
	if _, err := NewReadClone(root, "mem-project-b", bare); err == nil {
		t.Fatal("a project relation accepted a URL for mem-client-x")
	}
	if _, err := os.Stat(filepath.Join(root, "mem-project-b")); err == nil {
		t.Fatal("something was fetched")
	}
}

// Changing the configured URL after the first fetch is refused, not followed.
func TestReadClone_FetchRefusesAChangedURL(t *testing.T) {
	good := bareWithNotes(t, "mem-project-b", map[string]string{"a.md": "good"})
	other := filepath.Join(t.TempDir(), "mem-project-b.git")
	gitwt.New(filepath.Dir(other)).Run("clone", "--bare", good, other)
	quietBare(t, other)
	pushFiles(t, other, map[string]string{"a.md": "swapped"})
	root := t.TempDir()
	rc, _ := NewReadClone(root, "mem-project-b", good)
	if err := rc.Fetch(); err != nil {
		t.Fatal(err)
	}
	rc2, _ := NewReadClone(root, "mem-project-b", other)
	err := rc2.Fetch()
	if err == nil || !strings.Contains(err.Error(), "not the configured URL") {
		t.Fatalf("changed URL: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(rc.Dir(), "a.md")); string(b) != "good" {
		t.Fatalf("tree changed: %q", b)
	}
	if _, stale := rc2.Stale(); !stale {
		t.Fatal("a refused fetch is stale")
	}
}

// Fetched content never triggers a host filter or hook: nothing is checked out.
func TestReadClone_FetchedAttributesDoNotRunFilters(t *testing.T) {
	gitEnv(t)
	marker := filepath.Join(t.TempDir(), "RAN")
	gcfg := filepath.Join(t.TempDir(), "gitconfig")
	cfg := "[filter \"evil\"]\n\tsmudge = touch " + marker + "\n\tclean = touch " + marker + "\n\tprocess = touch " + marker + "\n\trequired = true\n"
	if err := os.WriteFile(gcfg, []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	bare := bareWithNotes(t, "mem-project-b", map[string]string{
		".gitattributes": "* filter=evil export-subst\n",
		"n.md":           "note $Format:%H$",
	})
	t.Setenv("GIT_CONFIG_GLOBAL", gcfg)
	rc, _ := NewReadClone(t.TempDir(), "mem-project-b", bare)
	if err := rc.Fetch(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a filter configured on this host ran on fetched content")
	}
	if b, _ := os.ReadFile(filepath.Join(rc.Dir(), "n.md")); !strings.HasPrefix(string(b), "note ") {
		t.Fatalf("export missing: %q", b)
	}
}

func writeScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fakessh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadClone_SSHIsBatchModeAndNeverPrompts(t *testing.T) {
	gitEnv(t)
	rec := filepath.Join(t.TempDir(), "rec")
	script := writeScript(t, `echo "$@" > `+rec+`; echo "PROMPT=$GIT_TERMINAL_PROMPT" >> `+rec+`; exit 1`)
	t.Setenv("GIT_SSH_COMMAND", script+" -o Existing=yes")
	rc, _ := NewReadClone(t.TempDir(), "mem-project-b", "git@alias:org/mem-project-b.git")
	if err := rc.Fetch(); err == nil {
		t.Fatal("fake ssh failing must fail the fetch")
	}
	b, _ := os.ReadFile(rec)
	for _, want := range []string{"Existing=yes", "BatchMode=yes", "PROMPT=0"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("ssh invocation lacks %s:\n%s", want, b)
		}
	}
}

func TestReadClone_FetchTimesOutAndIsStale(t *testing.T) {
	gitEnv(t)
	script := writeScript(t, `exec sleep 30`)
	t.Setenv("GIT_SSH_COMMAND", script)
	root := t.TempDir()
	rc, _ := NewReadClone(root, "mem-project-b", "git@alias:org/mem-project-b.git")
	rc.Timeout = 300 * time.Millisecond
	start := time.Now()
	err := rc.Fetch()
	if err == nil || time.Since(start) > 10*time.Second {
		t.Fatalf("no timeout: %v after %s", err, time.Since(start))
	}
	since, stale := rc.Stale()
	if !stale || since != "never" {
		t.Fatalf("stale = %q %v", since, stale)
	}
}

// hostileRemote builds a bare repo whose tip has a tree entry with the given
// name, made with plumbing so git's own add-time checks do not stand in the way.
func hostileRemote(t *testing.T, repo, entry string) string {
	t.Helper()
	gitEnv(t)
	root := t.TempDir()
	bare := filepath.Join(root, repo+".git")
	g := gitwt.New(root)
	if _, err := g.Run("init", "--bare", "-b", "main", bare); err != nil {
		t.Fatal(err)
	}
	b := gitwt.New(bare)
	oid, err := b.RunStdin([]byte("hostile"), "hash-object", "-w", "--stdin")
	if err != nil {
		t.Fatal(err)
	}
	tree, err := b.RunStdin([]byte("100644 blob "+strings.TrimSpace(oid)+"\t"+entry+"\x00"), "mktree", "-z")
	if err != nil {
		t.Fatal(err)
	}
	commit, err := b.Run("commit-tree", strings.TrimSpace(tree), "-m", "x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Run("update-ref", "refs/heads/main", strings.TrimSpace(commit)); err != nil {
		t.Fatal(err)
	}
	return bare
}

func TestReadClone_HostileTreeNamesAreRefusedAtFetch(t *testing.T) {
	for _, name := range []string{"git~1", ".git\u200c", ".GIT", "git~1 "} {
		bare := hostileRemote(t, "mem-project-b", name)
		root := t.TempDir()
		rc, _ := NewReadClone(root, "mem-project-b", bare)
		if err := rc.Fetch(); err == nil {
			t.Errorf("%q: fetch accepted a .git alias", name)
		}
		if _, err := os.Stat(rc.Dir()); err == nil {
			t.Errorf("%q: a tree was written", name)
		}
		ents, _ := os.ReadDir(root)
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), ".tmp-") {
				t.Errorf("%q: temp dir left behind", name)
			}
		}
	}
}

// Host config cannot relax the fetch checks.
func TestReadClone_HostConfigCannotRelaxFsck(t *testing.T) {
	gitEnv(t)
	gcfg := filepath.Join(t.TempDir(), "gitconfig")
	os.WriteFile(gcfg, []byte("[fetch]\n\tfsckObjects = false\n[transfer]\n\tfsckObjects = false\n[fsck]\n\thasDotgit = ignore\n[fetch \"fsck\"]\n\thasDotgit = ignore\n"), 0600)
	bare := hostileRemote(t, "mem-project-b", "git~1")
	t.Setenv("GIT_CONFIG_GLOBAL", gcfg)
	rc, _ := NewReadClone(t.TempDir(), "mem-project-b", bare)
	if err := rc.Fetch(); err == nil {
		t.Fatal("host config relaxed the object checks")
	}
}

func TestSafeRel(t *testing.T) {
	good := map[string]string{"a.md": "a.md", "projects/p/memory/n.md": "projects/p/memory/n.md", "x//y.md": "x/y.md", "./a.md": "a.md", ".gitignore": ".gitignore", "gitx/a": "gitx/a", ".github/w.yml": ".github/w.yml", "git~/a": "git~/a"}
	for in, want := range good {
		if got, ok := safeRel(in); !ok || got != want {
			t.Errorf("safeRel(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
	bad := []string{"", "..", "../x", "a/../b", "a/..", "/abs", "C:/x", "c:\\x", "a\\b", "a\\..\\b", "x\x00y",
		".git", ".git/config", "a/.git/hooks/x", ".GIT/x", ".Git", ".git.", ".git ", ".git. .", "a/.git /x",
		"git~1", "GIT~1/x", "git~12/x", "git~1.", ".g\u200cit", ".git\u200c", ".git\ufeff/x", ".git::$INDEX_ALLOCATION/x"}
	for _, in := range bad {
		if got, ok := safeRel(in); ok {
			t.Errorf("safeRel(%q) = %q, want refusal", in, got)
		}
	}
}

func TestReadClone_SymlinksAreNotExported(t *testing.T) {
	gitEnv(t)
	bare := bareWithNotes(t, "mem-project-b", map[string]string{"note.md": "ok"})
	work := t.TempDir()
	w := gitwt.New(work)
	if err := w.Clone(bare); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(work, "link.md")); err != nil {
		t.Skip("no symlinks")
	}
	w.AddAll()
	if _, err := w.Commit("link"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Run("push", "origin", "HEAD:main"); err != nil {
		t.Fatal(err)
	}
	rc, _ := NewReadClone(t.TempDir(), "mem-project-b", bare)
	if err := rc.Fetch(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(rc.Dir(), "link.md")); err == nil {
		t.Fatal("a symlink was exported")
	}
	if _, err := os.Stat(filepath.Join(rc.Dir(), "note.md")); err != nil {
		t.Fatal("regular file missing")
	}
}

func TestReadClone_SizeCap(t *testing.T) {
	big := strings.Repeat("x", 600)
	bare := bareWithNotes(t, "mem-project-b", map[string]string{"a.md": big, "b.md": big})
	root := t.TempDir()
	rc, _ := NewReadClone(root, "mem-project-b", bare)
	rc.MaxBytes = 1000
	err := rc.Fetch()
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("over the cap: %v", err)
	}
	if _, err := os.Stat(rc.Dir()); err == nil {
		t.Fatal("a refused tip was exported")
	}
	rc.MaxBytes = 1300
	if err := rc.Fetch(); err != nil {
		t.Fatalf("under the cap: %v", err)
	}
	// A refused later tip leaves the old copy in place.
	pushFiles(t, bare, map[string]string{"c.md": big})
	if err := rc.Fetch(); err == nil {
		t.Fatal("grown past the cap")
	}
	if _, err := os.Stat(filepath.Join(rc.Dir(), "a.md")); err != nil {
		t.Fatal("old copy lost")
	}
	// The default cap is 64 MB.
	rc2, _ := NewReadClone(t.TempDir(), "mem-project-b", bare)
	if err := rc2.Fetch(); err != nil {
		t.Fatal(err)
	}
}

// A hooks directory in the host's config never runs for a fetch.
func TestReadClone_HooksPathIsOff(t *testing.T) {
	gitEnv(t)
	hooks := t.TempDir()
	marker := filepath.Join(t.TempDir(), "RAN")
	os.WriteFile(filepath.Join(hooks, "reference-transaction"), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0700)
	gcfg := filepath.Join(t.TempDir(), "gitconfig")
	os.WriteFile(gcfg, []byte("[core]\n\thooksPath = "+hooks+"\n"), 0600)
	bare := bareWithNotes(t, "mem-project-b", map[string]string{"a.md": "x"})
	t.Setenv("GIT_CONFIG_GLOBAL", gcfg)
	// Control: the hook does fire for an ordinary ref update under this config.
	ctl := t.TempDir()
	if _, err := gitwt.New(ctl).Run("init", "--bare", "-q", "ctl.git"); err != nil {
		t.Fatal(err)
	}
	g := gitwt.New(filepath.Join(ctl, "ctl.git"))
	empty, _ := g.RunStdin(nil, "hash-object", "-t", "tree", "-w", "--stdin")
	commit, err := g.Run("commit-tree", strings.TrimSpace(empty), "-m", "c")
	if err != nil {
		t.Fatal(err)
	}
	g.Run("update-ref", "refs/heads/x", strings.TrimSpace(commit))
	if _, err := os.Stat(marker); err != nil {
		t.Skip("this git does not run reference-transaction hooks")
	}
	os.Remove(marker)
	rc, _ := NewReadClone(t.TempDir(), "mem-project-b", bare)
	if err := rc.Fetch(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a hook from the host's core.hooksPath ran during the fetch")
	}
}

// A fetch must not start auto maintenance: it may detach (setsid) and outlive
// the command's timeout.
func TestReadClone_FetchStartsNoAutoMaintenance(t *testing.T) {
	bare := bareWithNotes(t, "mem-other", map[string]string{"a.md": "a\n"})
	c, err := NewReadClone(t.TempDir(), "mem-other", bare)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Fetch(); err != nil {
		t.Fatal(err)
	}
	// Auto gc is due for sure: any pack beyond one counts as too many.
	for _, kv := range [][2]string{{"gc.auto", "1"}, {"gc.autoPackLimit", "1"}, {"fetch.unpackLimit", "1"}} {
		if _, err := gitwt.New(t.TempDir()).Run("--git-dir="+c.store, "config", kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	pushFiles(t, bare, map[string]string{"b.md": "b\n"})
	trace := filepath.Join(t.TempDir(), "trace")
	t.Setenv("GIT_TRACE", trace)
	if err := c.Fetch(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, "run_command") && (strings.Contains(l, "maintenance run") || strings.Contains(l, "gc --auto")) {
			t.Errorf("auto maintenance was started by the fetch: %s", l)
		}
	}
}
