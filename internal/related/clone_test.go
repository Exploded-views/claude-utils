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
	pushFiles(t, bare, files)
	return bare
}

func pushFiles(t *testing.T, bare string, files map[string]string) {
	t.Helper()
	work := t.TempDir()
	w := gitwt.New(work)
	if err := w.Clone(bare); err != nil {
		t.Fatal(err)
	}
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
