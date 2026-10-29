package gitwt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// maintenanceLines returns the GIT_TRACE lines that start auto maintenance:
// `maintenance run` or `gc --auto`. Such a child may be detached (setsid), so
// it escapes proc's process-group kill and outlives a caller's deadline.
func maintenanceLines(t *testing.T, trace string) []string {
	t.Helper()
	b, err := os.ReadFile(trace)
	if err != nil {
		t.Fatalf("no trace written: %v", err)
	}
	var got []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, "run_command") && (strings.Contains(l, "maintenance run") || strings.Contains(l, "gc --auto")) {
			got = append(got, l)
		}
	}
	return got
}

// A commit and a fetch through Repo must not start auto maintenance, detached
// or not.
func TestRunStartsNoAutoMaintenance(t *testing.T) {
	gitIdent(t)
	r := testRepo(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	if _, err := New(t.TempDir()).Run("init", "--bare", "-b", "main", remote); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run("remote", "add", "origin", remote); err != nil {
		t.Fatal(err)
	}
	// Make auto gc due for sure: any pack beyond one counts as too many.
	for _, kv := range [][2]string{{"gc.auto", "1"}, {"gc.autoPackLimit", "1"}, {"fetch.unpackLimit", "1"}, {"receive.unpackLimit", "1"}} {
		if _, err := r.Run("config", kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	write(t, r, "a.md", []byte("a\n"))
	if err := r.AddAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("first"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run("push", "origin", "main"); err != nil {
		t.Fatal(err)
	}

	trace := filepath.Join(t.TempDir(), "trace")
	t.Setenv("GIT_TRACE", trace)
	write(t, r, "b.md", []byte("b\n"))
	if err := r.AddAll(); err != nil {
		t.Fatal(err)
	}
	if made, err := r.Commit("second"); err != nil || !made {
		t.Fatalf("Commit = %v, %v", made, err)
	}
	if err := r.Fetch("main"); err != nil {
		t.Fatal(err)
	}
	if got := maintenanceLines(t, trace); len(got) > 0 {
		t.Errorf("auto maintenance was started by Repo.run:\n%s", strings.Join(got, "\n"))
	}
}
