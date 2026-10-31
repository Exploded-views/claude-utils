package distill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Links in a local memory dir are not followed into the synced catalog: a
// linked note, a linked memory folder and a linked project folder are skipped.
func TestAnalyzeSources_SkipsLinks(t *testing.T) {
	d := t.TempDir()
	proj := filepath.Join(d, "projects")
	cat := filepath.Join(d, "distilled")
	outside := filepath.Join(d, "outside")
	note := "---\nname: leaked\nmetadata:\n  scope: environment\n---\nbody\n"
	writeFile(t, filepath.Join(outside, "secret.md"), note)
	writeFile(t, filepath.Join(outside, "memory", "x.md"), note)
	os.MkdirAll(filepath.Join(proj, "p1", "memory"), 0o700)
	os.MkdirAll(filepath.Join(proj, "p2"), 0o700)
	os.MkdirAll(cat, 0o700)
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(proj, "p1", "memory", "linked.md")); err != nil {
		t.Skip(err)
	}
	os.Symlink(outside+"/memory", filepath.Join(proj, "p2", "memory"))
	os.Symlink(outside, filepath.Join(proj, "p3"))
	writeFile(t, filepath.Join(proj, "p1", "memory", "real.md"), note)
	res, err := BuildIndex(Options{ProjectsDir: proj, DistilledDir: cat})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Pending) != 1 || res.Pending[0].File != "real.md" {
		var got []string
		for _, p := range res.Pending {
			got = append(got, p.Project+"/"+p.File)
		}
		t.Errorf("pending = %s, want only p1/real.md", strings.Join(got, ", "))
	}
}

// A source that became a link says nothing about the entry: it is kept, and the
// link is not read.
func TestReconcileKeepsAnEntryWhoseSourceIsALink(t *testing.T) {
	d := t.TempDir()
	projects := filepath.Join(d, "projects")
	distilled := filepath.Join(d, "distilled")
	writeFile(t, filepath.Join(d, "outside", "x.md"), sourceMemory("x", false, "y")) // no marker: would read as stale
	os.MkdirAll(filepath.Join(projects, "P1", "memory"), 0o700)
	if err := os.Symlink(filepath.Join(d, "outside", "x.md"), filepath.Join(projects, "P1", "memory", "x.md")); err != nil {
		t.Skip(err)
	}
	writeFile(t, filepath.Join(distilled, "x.md"), catalogEntry("x", "d", "feedback", "P1", "x.md", "y"))
	res, err := Reconcile(Options{DistilledDir: distilled, ProjectsDir: projects})
	if err != nil || res.Pruned != 0 {
		t.Fatalf("pruned=%d err=%v, want the entry kept", res.Pruned, err)
	}
}
