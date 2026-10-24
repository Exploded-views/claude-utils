package gitwt

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func testRepo(t *testing.T) *Repo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	r := New(t.TempDir())
	if err := r.Init("main"); err != nil {
		t.Fatal(err)
	}
	return r
}

func write(t *testing.T, r *Repo, name string, content []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.Dir, name), content, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestBlobContents_BinaryEmptyAndNewlines(t *testing.T) {
	r := testRepo(t)
	want := map[string][]byte{
		"bin":   {0, 1, 2, '\n', '\n', 0xff},
		"empty": {},
		"text":  []byte("line one\nline two\n"),
	}
	for n, c := range want {
		write(t, r, n, c)
	}
	if err := r.AddAll(); err != nil {
		t.Fatal(err)
	}
	changes, err := r.StagedChanges()
	if err != nil || len(changes) != 3 {
		t.Fatalf("changes=%v err=%v", changes, err)
	}
	oids := make([]string, len(changes))
	for i, c := range changes {
		oids[i] = c.Oid
	}
	got, err := r.BlobContents(oids)
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range changes {
		if !bytes.Equal(got[i], want[c.Path]) {
			t.Errorf("%s: got %q, want %q", c.Path, got[i], want[c.Path])
		}
	}
}

func TestUnstage_LiteralAndFold(t *testing.T) {
	r := testRepo(t)
	for _, n := range []string{"[ab].md", "a.md", "Keep.TXT", "keep.txt"} {
		write(t, r, n, []byte("x\n"))
	}
	if err := r.AddAll(); err != nil {
		t.Fatal(err)
	}
	if err := r.Unstage("[ab].md"); err != nil {
		t.Fatal(err)
	}
	if err := r.UnstageFold("KEEP.txt"); err != nil {
		t.Fatal(err)
	}
	changes, _ := r.StagedChanges()
	if len(changes) != 1 || changes[0].Path != "a.md" {
		t.Errorf("only a.md should stay staged, got %v", changes)
	}
}
