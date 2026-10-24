package archive

import (
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

func TestPlace_WritesUnderDateAndFolder(t *testing.T) {
	root := t.TempDir()
	fixedDay(t, "2026-10-25")
	rel, err := Place(root, "p1", "note.md", []byte("body\n"))
	if err != nil {
		t.Fatal(err)
	}
	if rel != "archive/2026-10-25/p1/note.md" {
		t.Fatalf("rel = %q", rel)
	}
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil || string(b) != "body\n" {
		t.Fatalf("read: %q %v", b, err)
	}
}

func TestPlace_SameNameSameDayGetsSuffixNeverOverwrites(t *testing.T) {
	root := t.TempDir()
	fixedDay(t, "2026-10-25")
	r1, _ := Place(root, "p1", "note.md", []byte("one\n"))
	r2, err := Place(root, "p1", "note.md", []byte("two\n"))
	if err != nil {
		t.Fatal(err)
	}
	r3, _ := Place(root, "p1", "note.md", []byte("three\n"))
	if r1 == r2 || r2 == r3 || r1 == r3 {
		t.Fatalf("paths collide: %q %q %q", r1, r2, r3)
	}
	if !strings.HasSuffix(r2, ".md") || !strings.Contains(r2, "note") {
		t.Errorf("suffix should keep name and extension: %q", r2)
	}
	for rel, want := range map[string]string{r1: "one\n", r2: "two\n", r3: "three\n"} {
		if b, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel))); string(b) != want {
			t.Errorf("%s = %q, want %q", rel, b, want)
		}
	}
}

func TestPlace_RetryWithSameBytesReusesThePath(t *testing.T) {
	root := t.TempDir()
	fixedDay(t, "2026-10-25")
	r1, _ := Place(root, "p1", "note.md", []byte("one\n"))
	r2, _ := PlaceReuse(root, "p1", "note.md", []byte("one\n"), func(string) bool { return true })
	if r1 != r2 {
		t.Fatalf("a retry should land on the same path: %q vs %q", r1, r2)
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
