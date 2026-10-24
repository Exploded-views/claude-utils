package sync

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/merge"
)

// mainReconcile is Reconcile exactly as it was on main, before remote keys:
// the oracle that path mode must stay identical to.
func mainReconcile(r Roots, manifest *Manifest) (SyncReport, error) {
	rep := SyncReport{}
	if manifest == nil {
		manifest = NewManifest()
	}

	hashes, err := r.MemoryDirs()
	if err != nil {
		return rep, err
	}

	for _, hash := range hashes {
		claudeMem := filepath.Join(r.Claude, hash, "memory")
		mirrorMem := filepath.Join(r.Mirror, hash, "memory")
		if err := os.MkdirAll(mirrorMem, 0700); err != nil {
			return rep, err
		}

		files := map[string]struct{}{}
		if entries, err := os.ReadDir(claudeMem); err == nil {
			for _, e := range entries {
				if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
					continue
				}
				files[e.Name()] = struct{}{}
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return rep, fmt.Errorf("read claude %s: %w", claudeMem, err)
		}
		if entries, err := os.ReadDir(mirrorMem); err == nil {
			for _, e := range entries {
				if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
					continue
				}
				if strings.Contains(e.Name(), ".from-remote-") {
					continue
				}
				files[e.Name()] = struct{}{}
			}
		}

		for name := range files {
			cp := filepath.Join(claudeMem, name)
			mp := filepath.Join(mirrorMem, name)
			rel := filepath.ToSlash(filepath.Join(hash, "memory", name))

			cExists, cBytes := readIfExists(cp)
			mExists, mBytes := readIfExists(mp)
			wasInManifest := manifest.Has(rel)

			switch {
			case cExists && !mExists:
				if err := writeAtomic(mp, cBytes); err != nil {
					return rep, fmt.Errorf("write mirror %s: %w", mp, err)
				}
				rep.CopiedToMirror = append(rep.CopiedToMirror, rel)

			case !cExists && mExists && wasInManifest:
				if err := os.Remove(mp); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return rep, fmt.Errorf("remove mirror %s: %w", mp, err)
				}
				rep.RemovedFromMirror = append(rep.RemovedFromMirror, rel)

			case !cExists && mExists:
				if err := os.MkdirAll(claudeMem, 0700); err != nil {
					return rep, err
				}
				if err := writeAtomic(cp, mBytes); err != nil {
					return rep, fmt.Errorf("write claude %s: %w", cp, err)
				}
				rep.CopiedToClaude = append(rep.CopiedToClaude, rel)

			case cExists && mExists && bytes.Equal(cBytes, mBytes):

			case cExists && mExists && !bytes.Equal(cBytes, mBytes):
				if name == "MEMORY.md" {
					merged, _ := merge.Merge("", string(cBytes), string(mBytes))
					if err := writeAtomic(cp, []byte(merged)); err != nil {
						return rep, err
					}
					if err := writeAtomic(mp, []byte(merged)); err != nil {
						return rep, err
					}
					rep.Merged = append(rep.Merged, rel)
				} else {
					backup := fmt.Sprintf("%s.from-remote-%s", mp, randSuffix())
					if err := writeAtomic(backup, mBytes); err != nil {
						return rep, err
					}
					if err := writeAtomic(mp, cBytes); err != nil {
						return rep, err
					}
					rep.CopiedToMirror = append(rep.CopiedToMirror, rel)
					rep.BackedUp = append(rep.BackedUp, rel)
				}
			}
		}
	}
	return rep, nil
}

// treeSnap is every file under a root: content and modification time.
type treeSnap map[string]struct {
	content string
	mtime   time.Time
}

func snapTree(t *testing.T, root string) treeSnap {
	t.Helper()
	out := treeSnap{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			out[rel+"/"] = struct {
				content string
				mtime   time.Time
			}{}
			return nil
		}
		// Backups carry a random suffix; the content is what matters.
		if i := strings.Index(rel, ".from-remote-"); i >= 0 {
			rel = rel[:i] + ".from-remote-*" + "#" + fmt.Sprint(len(out))
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out[rel] = struct {
			content string
			mtime   time.Time
		}{string(b), info.ModTime()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sortedReport(rep SyncReport) map[string][]string {
	s := func(in []string) []string {
		out := append([]string(nil), in...)
		sort.Strings(out)
		return out
	}
	return map[string][]string{
		"CopiedToMirror": s(rep.CopiedToMirror), "CopiedToClaude": s(rep.CopiedToClaude), "Merged": s(rep.Merged),
		"BackedUp": s(rep.BackedUp), "RemovedFromMirror": s(rep.RemovedFromMirror),
	}
}

// pathFixture builds, in a fresh temp dir, a path-mode tree that exercises every
// rule of Reconcile, with every file's mtime set far in the past so that any
// rewrite shows. It returns the roots and the manifest to reconcile with.
func pathFixture(t *testing.T) (Roots, *Manifest) {
	t.Helper()
	r, _ := reconcileFixture(t)
	old := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	put := func(root, hash, name, content string) {
		writeMem(t, root, hash, name, content)
		p := filepath.Join(root, hash, "memory", name)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	idxA := "# Mem\n\n## Shared\ns\n"
	// proj1: identical files on both sides, claude-only, mirror-only, a delete,
	// a non-index collision and an index collision.
	put(r.Claude, "proj1", "same.md", "same\n")
	put(r.Mirror, "proj1", "same.md", "same\n")
	put(r.Claude, "proj1", "MEMORY.md", idxA+"\n## Local\nl\n")
	put(r.Mirror, "proj1", "MEMORY.md", idxA+"\n## Remote\nr\n")
	put(r.Claude, "proj1", "only_claude.md", "c\n")
	put(r.Mirror, "proj1", "only_mirror.md", "m\n")
	put(r.Mirror, "proj1", "deleted.md", "d\n")
	put(r.Claude, "proj1", "collide.md", "claude version\n")
	put(r.Mirror, "proj1", "collide.md", "mirror version\n")
	// Skipped names: dotfiles, and backups from earlier collisions.
	put(r.Claude, "proj1", ".hidden", "h\n")
	put(r.Mirror, "proj1", ".hidden2", "h\n")
	put(r.Mirror, "proj1", "old.md.from-remote-deadbeef", "b\n")
	// A subdirectory in memory/ is not a memory file.
	if err := os.MkdirAll(filepath.Join(r.Claude, "proj1", "memory", "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	// proj2: an index that is a strict subset of the other side's.
	put(r.Claude, "proj2", "MEMORY.md", idxA)
	put(r.Mirror, "proj2", "MEMORY.md", idxA+"\n## More\nm\n")
	// proj3: new from another PC (mirror only), and one whose files all were
	// deleted here.
	put(r.Mirror, "proj3", "MEMORY.md", idxA)
	put(r.Mirror, "proj3", "n.md", "n\n")
	put(r.Mirror, "proj4", "gone.md", "g\n")
	// proj5: a Claude folder with transcripts only (no memory/).
	if err := os.MkdirAll(filepath.Join(r.Claude, "proj5"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.Claude, "proj5", "session.jsonl"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// A stray file at the top of each root is not a project.
	if err := os.WriteFile(filepath.Join(r.Claude, "stray.txt"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}

	man := NewManifest()
	for _, rel := range []string{
		"proj1/memory/same.md", "proj1/memory/deleted.md", "proj1/memory/only_claude.md",
		"proj4/memory/gone.md", "proj9/memory/nowhere.md",
	} {
		man.Add(rel)
	}
	return r, man
}

// Path mode must do exactly what main did. The oracle above is main's
// Reconcile; both run on identical trees and must end with byte-identical
// files and reports, and must leave untouched every file main left untouched
// (same modification time), in both trees.
func TestReconcile_PathModeMatchesMain(t *testing.T) {
	for _, withManifest := range []bool{true, false} {
		t.Run(fmt.Sprintf("manifest=%v", withManifest), func(t *testing.T) {
			rOurs, man := pathFixture(t)
			rMain, _ := pathFixture(t)
			if !withManifest {
				man = nil
			}

			repOurs, err := Reconcile(rOurs, man)
			if err != nil {
				t.Fatal(err)
			}
			repMain, err := mainReconcile(rMain, man)
			if err != nil {
				t.Fatal(err)
			}

			if a, b := sortedReport(repOurs), sortedReport(repMain); fmt.Sprint(a) != fmt.Sprint(b) {
				t.Errorf("report differs from main:\n ours: %v\n main: %v", a, b)
			}
			old := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
			for _, side := range []struct{ name, ours, main string }{
				{"claude", rOurs.Claude, rMain.Claude},
				{"mirror", rOurs.Mirror, rMain.Mirror},
			} {
				got, want := snapTree(t, side.ours), snapTree(t, side.main)
				for p, w := range want {
					g, ok := got[p]
					if !ok {
						t.Errorf("%s: %s exists on main but not here", side.name, p)
						continue
					}
					if g.content != w.content {
						t.Errorf("%s: %s = %q, main has %q", side.name, p, g.content, w.content)
					}
					if w.mtime.Equal(old) && !g.mtime.Equal(old) {
						t.Errorf("%s: %s was rewritten (mtime %v); main left it untouched", side.name, p, g.mtime)
					}
				}
				for p := range got {
					if _, ok := want[p]; !ok {
						t.Errorf("%s: %s exists here but not on main", side.name, p)
					}
				}
			}
		})
	}
}

// mainPropagateChanges is propagateChanges exactly as it was on main, before
// remote keys: the oracle that path mode must stay identical to.
func mainPropagateChanges(repo *gitwt.Repo, roots Roots, pre, post string) error {
	out, err := repo.Run("diff", "--name-status", pre+".."+post)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		status := fields[0]
		var path string
		if strings.HasPrefix(status, "R") && len(fields) >= 3 {
			oldPath := fields[1]
			path = fields[2]
			if hash, name, ok := splitMirrorPath(oldPath); ok {
				_ = os.Remove(filepath.Join(roots.Claude, hash, "memory", name))
			}
		} else {
			path = fields[1]
		}
		hash, name, ok := splitMirrorPath(path)
		if !ok {
			continue
		}
		switch {
		case strings.HasPrefix(status, "D"):
			_ = os.Remove(filepath.Join(roots.Claude, hash, "memory", name))
		default:
			if err := CopyToClaude(roots, hash, name); err != nil {
				log.Printf("propagate %s: %v", path, err)
			}
		}
	}
	return nil
}

// A pull that adds, modifies, deletes and renames files must reach Claude's
// tree in path mode exactly as it did on main. Both versions replay the same
// real git history onto identical Claude trees.
func TestPropagateChanges_PathModeMatchesMain(t *testing.T) {
	gitEnv(t)
	shared := t.TempDir()
	mirror := filepath.Join(shared, "projects")
	sr := gitwt.New(shared)
	if err := sr.Init("main"); err != nil {
		t.Fatal(err)
	}
	commit := func(msg string) string {
		t.Helper()
		if err := sr.AddAll(); err != nil {
			t.Fatal(err)
		}
		if _, err := sr.Commit(msg); err != nil {
			t.Fatal(err)
		}
		head, err := sr.Run("rev-parse", "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(head)
	}
	body := strings.Repeat("a line long enough for git to see a rename\n", 8)
	for _, f := range []struct{ hash, name, content string }{
		{"proj1", "keep.md", "keep\n"}, {"proj1", "edit.md", "before\n"}, {"proj1", "drop.md", "drop\n"},
		{"proj1", "old-name.md", body}, {"proj2", "other.md", "other\n"}, {"proj3", "unseen.md", "u\n"},
	} {
		writeMem(t, shared, "projects/"+f.hash, f.name, f.content)
	}
	pre := commit("one")

	rewrite := func(hash, name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(mirror, hash, "memory", name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	rewrite("proj1", "edit.md", "after\n")                                                 // M
	rewrite("proj1", "new.md", "new\n")                                                    // A
	rewrite("proj2", "added-elsewhere.md", "x\n")                                          // A in another project
	if err := os.Remove(filepath.Join(mirror, "proj1", "memory", "drop.md")); err != nil { // D
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(mirror, "proj1", "memory", "old-name.md"), // R
		filepath.Join(mirror, "proj1", "memory", "new-name.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shared, "README.md"), []byte("not a memory\n"), 0600); err != nil { // not a memory path
		t.Fatal(err)
	}
	post := commit("two")

	status, err := sr.Run("diff", "--name-status", pre+".."+post)
	if err != nil {
		t.Fatal(err)
	}
	for _, letter := range []string{"A\t", "M\t", "D\t", "R"} {
		if !strings.Contains("\n"+status, "\n"+letter) {
			t.Fatalf("setup: the history has no %q entry:\n%s", letter, status)
		}
	}

	claudeTree := func() Roots {
		t.Helper()
		claude := t.TempDir()
		for _, f := range []struct{ hash, name, content string }{
			{"proj1", "keep.md", "keep\n"}, {"proj1", "edit.md", "before\n"}, {"proj1", "drop.md", "drop\n"},
			{"proj1", "old-name.md", body}, {"proj1", "local-only.md", "mine\n"}, {"proj2", "other.md", "other\n"},
		} {
			writeMem(t, claude, f.hash, f.name, f.content)
		}
		return Roots{Claude: claude, Mirror: mirror}
	}
	ours, main := claudeTree(), claudeTree()
	if err := propagateChanges(sr, ours, pre, post); err != nil {
		t.Fatal(err)
	}
	if err := mainPropagateChanges(sr, main, pre, post); err != nil {
		t.Fatal(err)
	}
	got, want := snapTree(t, ours.Claude), snapTree(t, main.Claude)
	for p, w := range want {
		if g, ok := got[p]; !ok {
			t.Errorf("%s exists on main but not here", p)
		} else if g.content != w.content {
			t.Errorf("%s = %q, main has %q", p, g.content, w.content)
		}
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			t.Errorf("%s exists here but not on main", p)
		}
	}
	// And the oracle really did something with each kind of entry.
	for _, rel := range []string{"proj1/memory/new.md", "proj1/memory/new-name.md", "proj2/memory/added-elsewhere.md"} {
		if _, ok := want[rel]; !ok {
			t.Errorf("setup: main's result lacks %s", rel)
		}
	}
	for _, rel := range []string{"proj1/memory/drop.md", "proj1/memory/old-name.md"} {
		if _, ok := want[rel]; ok {
			t.Errorf("setup: main's result still has %s", rel)
		}
	}
}
