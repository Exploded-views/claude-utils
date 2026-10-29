package changes

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/proc"
)

func (f *fixture) markerOpts(marker string) Options {
	return Options{SyncDir: f.repo.Dir, ClaudeDir: f.claude, Marker: marker, UpdateMarker: true, DeferMarker: true, Limit: 20}
}

func spawns(f func()) int {
	var n atomic.Int64
	restore := proc.SetSpawnHook(func(string, []string) { n.Add(1) })
	defer restore()
	f()
	return int(n.Load())
}

// A marker at HEAD means nothing is new: the answer needs no git process. A
// commit after that is reported at once (the skip never goes stale), and the
// marker is then moved as before.
func TestMarkerAtHeadNeedsNoGitAndNeverGoesStale(t *testing.T) {
	f := newFixture(t)
	head := f.commit(map[string]string{mem(appHash, "MEMORY.md"): "# one\n"})
	marker := filepath.Join(t.TempDir(), "marker")
	if err := WriteMarker(marker, head); err != nil {
		t.Fatal(err)
	}
	var res Result
	var err error
	if n := spawns(func() { res, err = Compute(f.markerOpts(marker)) }); n != 0 {
		t.Errorf("marker at HEAD started %d git processes, want 0", n)
	}
	if err != nil || res.Total != 0 || res.Head != head || res.Since != head || res.Changes == nil {
		t.Fatalf("unchanged: %+v %v", res, err)
	}
	if err := SaveMarker(f.repo, marker, res); err != nil {
		t.Fatal(err)
	}

	// The repo really changes after the skip-eligible run.
	head2 := f.commit(map[string]string{mem(appHash, "new.md"): "n\n"})
	res, err = Compute(f.markerOpts(marker))
	if err != nil || res.Total != 1 || res.Head != head2 || find(res, "new.md") == nil {
		t.Fatalf("a commit after the skip was not reported: %+v %v", res, err)
	}
	if err := SaveMarker(f.repo, marker, res); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(marker); markerLine(b) != head2 {
		t.Errorf("marker = %q, want %s", b, head2)
	}
	// And once reported, nothing new again.
	if res, err = Compute(f.markerOpts(marker)); err != nil || res.Total != 0 {
		t.Fatalf("after the report: %+v %v", res, err)
	}
}

// The synced head bounds the report: a marker at it is current although HEAD is
// ahead, and a record that moves on is reported.
func TestMarkerAtSyncedHeadNeedsNoGit(t *testing.T) {
	f := newFixture(t)
	h1 := f.commit(map[string]string{mem(appHash, "MEMORY.md"): "# one\n"})
	h2 := f.commit(map[string]string{mem(appHash, "new.md"): "n\n"})
	marker := filepath.Join(t.TempDir(), "marker")
	if err := WriteMarker(marker, h1); err != nil {
		t.Fatal(err)
	}
	o := f.markerOpts(marker)
	o.Upper = h1
	var res Result
	var err error
	if n := spawns(func() { res, err = Compute(o) }); n != 0 || err != nil || res.Total != 0 || res.Head != h1 {
		t.Fatalf("marker at the synced head: %d spawns, %+v %v", n, res, err)
	}
	o.Upper = h2 // the daemon copied the new commit
	if res, err = Compute(o); err != nil || res.Total != 1 || res.Head != h2 {
		t.Fatalf("synced head moved on: %+v %v", res, err)
	}
}

// A repo stopped half-way through a merge is busy, whatever the marker says.
func TestMarkerAtHeadStillSaysBusy(t *testing.T) {
	f := newFixture(t)
	head := f.commit(map[string]string{mem(appHash, "MEMORY.md"): "# one\n"})
	marker := filepath.Join(t.TempDir(), "marker")
	if err := WriteMarker(marker, head); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.repo.Dir, ".git", "MERGE_HEAD"), []byte(head+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if res, err := Compute(f.markerOpts(marker)); err != nil || !res.Busy {
		t.Fatalf("busy repo: %+v %v", res, err)
	}
}

// HEAD in a packed ref, detached, or a branch other than main still reads right.
func TestReadHeadSHAForms(t *testing.T) {
	f := newFixture(t)
	head := f.commit(map[string]string{mem(appHash, "MEMORY.md"): "# one\n"})
	if got := readHeadSHA(f.repo.Dir); got != head {
		t.Fatalf("loose ref: %q want %q", got, head)
	}
	if _, err := f.repo.Run("pack-refs", "--all", "--prune"); err != nil {
		t.Fatal(err)
	}
	if got := readHeadSHA(f.repo.Dir); got != head {
		t.Fatalf("packed ref: %q want %q", got, head)
	}
	if _, err := f.repo.Run("checkout", "-q", "--detach"); err != nil {
		t.Fatal(err)
	}
	if got := readHeadSHA(f.repo.Dir); got != head {
		t.Fatalf("detached: %q want %q", got, head)
	}
}
