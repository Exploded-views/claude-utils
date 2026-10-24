package related

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// bareWithNotes makes a local bare remote holding the given files.
func bareWithNotes(t *testing.T, files map[string]string) (bare string) {
	t.Helper()
	gitEnv(t)
	root := t.TempDir()
	bare = filepath.Join(root, "remote.git")
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
	bare := bareWithNotes(t, map[string]string{"projects/p/memory/a.md": "one"})
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
	// A stray local edit in the clone is discarded.
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
	ch, ok := rc.ChangedSince(before)
	if !ok || len(ch) != 1 || ch[0].Status != "added" || ch[0].Path != "projects/p/memory/b.md" {
		t.Fatalf("ChangedSince = %+v %v", ch, ok)
	}
	if _, ok := rc.ChangedSince("deadbeef"); ok {
		t.Fatal("unknown commit reported as a baseline")
	}
}

// The read clone type has no way to write to the remote, and git refuses a
// push by hand even though the remote itself is writable.
func TestReadClone_NeverPushes(t *testing.T) {
	bare := bareWithNotes(t, map[string]string{"projects/p/memory/a.md": "one"})
	rc, _ := NewReadClone(t.TempDir(), "mem-project-b", bare)
	if err := rc.Fetch(); err != nil {
		t.Fatal(err)
	}
	typ := reflect.TypeOf(rc)
	for i := 0; i < typ.NumMethod(); i++ {
		n := strings.ToLower(typ.Method(i).Name)
		for _, bad := range []string{"push", "commit", "write", "run", "remote"} {
			if strings.Contains(n, bad) {
				t.Errorf("ReadClone has method %s", typ.Method(i).Name)
			}
		}
	}
	before := refs(t, bare)
	g := gitwt.New(rc.Dir())
	os.WriteFile(filepath.Join(rc.Dir(), "x.md"), []byte("x"), 0600)
	g.AddAll()
	g.Run("commit", "-m", "sneaky")
	if _, err := g.Run("push", "origin", "HEAD:main"); err == nil {
		t.Fatal("a manual push reached the writable remote")
	}
	if refs(t, bare) != before {
		t.Fatal("the remote changed")
	}
	// Fetching again also repairs the sneaky local commit.
	if err := rc.Fetch(); err != nil {
		t.Fatal(err)
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
