package related

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0700)
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestBuildIndex_CountsNotesAndReadsHeadingsOnly(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"projects/p/memory/MEMORY.md": "# Alpha\n- [n1](n1.md) — hook text BODYLINE\n## Beta\x1b[31m\n",
		"projects/p/memory/n1.md":     "SECRET BODY",
		"projects/p/memory/n2.md":     "SECRET BODY",
		"archive/2026-01-01/old.md":   "old",
		"README.md":                   "# readme",
		".git/x.md":                   "no",
	})
	ix, err := BuildIndex(dir, PlainOpener{})
	if err != nil {
		t.Fatal(err)
	}
	if ix.Notes != 2 {
		t.Errorf("notes = %d, want 2", ix.Notes)
	}
	if len(ix.Headings) != 2 || ix.Headings[0] != "# Alpha" || strings.ContainsRune(ix.Headings[1], 0x1b) {
		t.Errorf("headings = %q", ix.Headings)
	}
}

func TestRender_NamesAndHeadingsNeverBodies(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"projects/p/memory/MEMORY.md": "# Alpha\n- [n1](n1.md) — BODYLINE\n",
		"projects/p/memory/n1.md":     "SECRET BODY",
	})
	ix, _ := BuildIndex(dir, PlainOpener{})
	out := Render([]Block{{Repo: "mem-project-b", Path: dir, Index: ix}}, DefaultBudgets)
	for _, want := range []string{"mem-project-b, 1 note, read-only at " + dir, "# Alpha"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"SECRET", "BODYLINE"} {
		if strings.Contains(out, bad) {
			t.Errorf("leaks %q", bad)
		}
	}
}

func TestRender_Budgets(t *testing.T) {
	var hs []string
	for i := 0; i < 100; i++ {
		hs = append(hs, "# heading number "+strings.Repeat("x", 20))
	}
	mk := func(n string) Block {
		return Block{Repo: n, Path: "/p/" + n, Index: Index{Notes: 3, Headings: hs}}
	}
	one := Render([]Block{mk("mem-project-a")}, Budgets{PerRelation: 300, Total: 6000})
	// header line + one block of at most 300 runes
	if n := len([]rune(one)); n > 300+len([]rune(contextHeader))+2 {
		t.Errorf("per-relation budget exceeded: %d", n)
	}
	if !strings.Contains(one, "…") || !strings.Contains(one, "mem-project-a, 3 notes") {
		t.Errorf("truncation not marked or header cut:\n%s", one)
	}
	all := Render([]Block{mk("mem-project-a"), mk("mem-project-b"), mk("mem-project-c"), mk("mem-project-d")},
		Budgets{PerRelation: 400, Total: 900})
	if n := len([]rune(all)); n > 900+120 { // total plus the omission note
		t.Errorf("total budget exceeded: %d", n)
	}
	if !strings.Contains(all, "left out: context budget") {
		t.Errorf("no omission note:\n%s", all)
	}
	if strings.Contains(all, "mem-project-d, 3 notes") {
		t.Errorf("last relation should not fit:\n%s", all)
	}
}

func TestRender_Empty(t *testing.T) {
	if Render(nil, DefaultBudgets) != "" {
		t.Fatal("no blocks, some output")
	}
}

type fakeAge struct{}

func (fakeAge) Readable(dir string) bool { return true }
func (fakeAge) ReadFile(p string) ([]byte, error) {
	b, err := os.ReadFile(p)
	return []byte(strings.ReplaceAll(string(b), "CIPHER:", "")), err
}

func TestOpener_EncryptedUnreadableByDefault(t *testing.T) {
	dir := writeTree(t, map[string]string{EncryptedMarker: "age1x\n", "MEMORY.md": "CIPHER:# Hidden\n"})
	if (PlainOpener{}).Readable(dir) {
		t.Fatal("an encrypted clone must not be readable by the plain opener")
	}
	ix, err := BuildIndex(dir, fakeAge{})
	if err != nil || len(ix.Headings) != 1 || ix.Headings[0] != "# Hidden" {
		t.Fatalf("hook opener not used: %+v %v", ix, err)
	}
}
