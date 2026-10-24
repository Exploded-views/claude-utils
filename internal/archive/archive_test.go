package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixedDay(t *testing.T, day string) {
	t.Helper()
	d, err := time.Parse("2006-01-02", day)
	if err != nil {
		t.Fatal(err)
	}
	old := Now
	Now = func() time.Time { return d.Add(9 * time.Hour) }
	t.Cleanup(func() { Now = old })
}

func sha8(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])[:8]
}

func TestPlace_PathCarriesDateFolderAndContentHash(t *testing.T) {
	root := t.TempDir()
	fixedDay(t, "2026-10-25")
	rel, err := Place(root, "p1", "note.md", []byte("body\n"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "archive/2026-10-25/p1/note." + sha8([]byte("body\n")) + ".md"; rel != want {
		t.Fatalf("rel = %q, want %q", rel, want)
	}
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil || string(b) != "body\n" {
		t.Fatalf("read: %q %v", b, err)
	}
	rel2, _ := Place(root, "p1", "README", []byte("x"))
	if want := "archive/2026-10-25/p1/README." + sha8([]byte("x")); rel2 != want {
		t.Errorf("no extension: %q, want %q", rel2, want)
	}
}

// Identical copies collapse to one path (two hosts archiving the same note make
// one identical add); different content never shares a path, so nothing is
// overwritten and nothing collides.
func TestPlace_SameBytesSamePath_DifferentBytesDifferentPath(t *testing.T) {
	root := t.TempDir()
	fixedDay(t, "2026-10-25")
	r1, _ := Place(root, "p1", "note.md", []byte("one\n"))
	r1b, err := Place(root, "p1", "note.md", []byte("one\n"))
	if err != nil || r1 != r1b {
		t.Fatalf("identical bytes should land on one path: %q %q %v", r1, r1b, err)
	}
	r2, _ := Place(root, "p1", "note.md", []byte("two\n"))
	if r1 == r2 {
		t.Fatal("different bytes share a path")
	}
	for rel, want := range map[string]string{r1: "one\n", r2: "two\n"} {
		if b, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel))); string(b) != want {
			t.Errorf("%s = %q, want %q", rel, b, want)
		}
	}
}

func TestPlace_NeverOverwritesAHashCollision(t *testing.T) {
	root := t.TempDir()
	fixedDay(t, "2026-10-25")
	// Plant a different file at the path the content would take.
	rel := "archive/2026-10-25/p1/note." + sha8([]byte("one\n")) + ".md"
	p := filepath.Join(root, filepath.FromSlash(rel))
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	_ = os.WriteFile(p, []byte("planted\n"), 0o600)
	got, err := Place(root, "p1", "note.md", []byte("one\n"))
	if err != nil || got == rel {
		t.Fatalf("got %q err %v", got, err)
	}
	if b, _ := os.ReadFile(p); string(b) != "planted\n" {
		t.Error("planted file overwritten")
	}
}

func TestAppendIndex_AppendsOneLineAndIsIdempotentPerPath(t *testing.T) {
	root := t.TempDir()
	e := Entry{Date: "2026-10-25", Folder: "p1", File: "note.md", Reason: ReasonDeleted, Path: "archive/2026-10-25/p1/note.md"}
	if err := AppendIndex(root, e); err != nil {
		t.Fatal(err)
	}
	e2 := Entry{Date: "2026-10-25", Folder: "p1", File: "other.md", Reason: ReasonDeleted, Path: "archive/2026-10-25/p1/other.md"}
	if err := AppendIndex(root, e2); err != nil {
		t.Fatal(err)
	}
	if err := AppendIndex(root, e); err != nil { // retry
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(root, "archive", "INDEX.md"))
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d:\n%s", len(lines), b)
	}
	for _, want := range []string{"2026-10-25", "p1", "note.md", "deleted on a host"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("line %q lacks %q", lines[0], want)
		}
	}
}

func seed(t *testing.T, root string) {
	t.Helper()
	for rel, body := range map[string]string{
		"archive/2026-10-20/p1/old.md":           "alpha\nneedle here\nneedle again\n",
		"archive/2026-10-25/p1/new.md":           "beta\n",
		"archive/2026-10-25/distilled/needle.md": "gamma\n",
		"archive/INDEX.md":                       "- not an archived note, needle\n",
	} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestList_SinceFiltersByDate(t *testing.T) {
	root := t.TempDir()
	seed(t, root)
	all, err := List(root, "")
	if err != nil || len(all) != 3 {
		t.Fatalf("all: %v %v", all, err)
	}
	got, err := List(root, "2026-10-22")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Date != "2026-10-25" {
		t.Fatalf("since: %+v", got)
	}
	if _, err := List(root, "yesterday"); err == nil {
		t.Error("a bad --since date should be an error")
	}
	if e, err := List(t.TempDir(), ""); err != nil || len(e) != 0 {
		t.Errorf("no archive dir: %v %v", e, err)
	}
}

func TestSearch_NamesAndContents_NoIndex(t *testing.T) {
	root := t.TempDir()
	seed(t, root)
	hits, err := Search(root, "NEEDLE")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, h := range hits {
		got[h.Path] = len(h.Lines)
		if h.Path == "archive/INDEX.md" {
			t.Error("INDEX.md is not an archived note")
		}
	}
	if got["archive/2026-10-20/p1/old.md"] != 2 {
		t.Errorf("content hits: %v", got)
	}
	if n, ok := got["archive/2026-10-25/distilled/needle.md"]; !ok || n != 0 {
		t.Errorf("name-only hit should be listed with no lines: %v", got)
	}
	if _, ok := got["archive/2026-10-25/p1/new.md"]; ok {
		t.Error("non-matching file listed")
	}
}

func TestSearch_UsesTheSwappableReader(t *testing.T) {
	root := t.TempDir()
	seed(t, root)
	old := Read
	Read = func(string) ([]byte, error) { return []byte("decrypted needle\n"), nil }
	t.Cleanup(func() { Read = old })
	hits, err := Search(root, "decrypted")
	if err != nil || len(hits) != 3 {
		t.Fatalf("hits %v err %v", hits, err)
	}
}

// A symlink inside the archive is neither listed nor read: a search must never
// read outside archive/.
func TestListAndSearch_SkipSymlinks(t *testing.T) {
	root := t.TempDir()
	seed(t, root)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	_ = os.WriteFile(outside, []byte("needle outside\n"), 0o600)
	link := filepath.Join(root, "archive", "2026-10-25", "p1", "link.md")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("no symlinks here")
	}
	dirLink := filepath.Join(root, "archive", "2026-10-26")
	if err := os.Symlink(filepath.Dir(outside), dirLink); err != nil {
		t.Skip("no symlinks here")
	}
	entries, _ := List(root, "")
	for _, e := range entries {
		if strings.Contains(e.Path, "link") || strings.Contains(e.Path, "2026-10-26") || strings.Contains(e.Path, "secret") {
			t.Errorf("listed through a symlink: %s", e.Path)
		}
	}
	hits, err := Search(root, "outside")
	if err != nil || len(hits) != 0 {
		t.Errorf("search read outside the archive: %v %v", hits, err)
	}
}
