//go:build !windows

package sync

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const linkSecret = "SECRET-OUTSIDE-THE-TREE"

// A fetched tree that carries a symlink or a gitlink anywhere is refused before
// it is merged, and nothing outside the tree is read: a link in the mirror
// would otherwise be followed when the mirror is copied into the Claude tree.
func TestPull_RefusesSymlinkAndGitlinkEntries(t *testing.T) {
	type form struct {
		name  string
		plant func(t *testing.T, peer, outside string)
	}
	link := func(rel, target string) func(*testing.T, string, string) {
		return func(t *testing.T, peer, outside string) {
			p := filepath.Join(peer, filepath.FromSlash(rel))
			os.MkdirAll(filepath.Dir(p), 0o755)
			if err := os.Symlink(target, p); err != nil {
				t.Skip(err)
			}
		}
	}
	forms := []form{
		{"file link", func(t *testing.T, peer, outside string) {
			link("projects/p1/memory/note.md", filepath.Join(outside, "secret.txt"))(t, peer, outside)
		}},
		{"memory dir link", func(t *testing.T, peer, outside string) {
			link("projects/p2/memory", outside)(t, peer, outside)
		}},
		{"project dir link", func(t *testing.T, peer, outside string) {
			link("projects/p3", outside)(t, peer, outside)
		}},
		{"config.json link", func(t *testing.T, peer, outside string) {
			link("config.json", filepath.Join(outside, "secret.txt"))(t, peer, outside)
		}},
		{".state link", func(t *testing.T, peer, outside string) {
			link(".state", outside)(t, peer, outside)
		}},
		{"relative link", link("projects/p1/memory/rel.md", "../../../../secret-rel.txt")},
	}
	for _, fm := range forms {
		t.Run(fm.name, func(t *testing.T) {
			f := newPullFixture(t, map[string]string{"clean.md": "a\n"})
			captureLog(t)
			outside := t.TempDir()
			os.WriteFile(filepath.Join(outside, "secret.txt"), []byte(linkSecret), 0o600)
			os.WriteFile(filepath.Join(outside, "note.md"), []byte(linkSecret), 0o600)
			mustGit(t, f.peer, "pull", "-q", "origin", "main")
			fm.plant(t, f.peer.Dir, outside)
			mustGit(t, f.peer, "add", "-f", "-A")
			mustGit(t, f.peer, "commit", "-q", "-m", "links")
			mustGit(t, f.peer, "push", "-q", "origin", "main")

			f.loop.pullTick(f.repo, f.roots)
			if err := runPull(f.repo, "main"); !errors.Is(err, ErrLocalOnlyTracked) {
				t.Fatalf("a fetched link must be refused, got %v", err)
			}
			if strings.Contains(headFiles(t, f.repo), "links") {
				t.Error("HEAD took the links commit")
			}
			filepath.WalkDir(f.roots.Claude, func(p string, d fs.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					if b, _ := os.ReadFile(p); strings.Contains(string(b), linkSecret) {
						t.Errorf("the target's content reached the Claude tree: %s", p)
					}
				}
				return nil
			})
		})
	}
}

func TestPull_RefusesGitlinkEntries(t *testing.T) {
	f := newPullFixture(t, map[string]string{"clean.md": "a\n"})
	captureLog(t)
	mustGit(t, f.peer, "pull", "-q", "origin", "main")
	mustGit(t, f.peer, "update-index", "--add", "--cacheinfo", "160000,"+strings.TrimSpace(f.head())+",projects/p1/sub")
	mustGit(t, f.peer, "commit", "-q", "-m", "gitlink")
	mustGit(t, f.peer, "push", "-q", "origin", "main")
	if err := runPull(f.repo, "main"); !errors.Is(err, ErrLocalOnlyTracked) {
		t.Fatalf("a fetched gitlink must be refused, got %v", err)
	}
}

// A link that arrives in a later commit, after a clean first fetch, is refused
// the same way, and the merge that already happened stays clean.
func TestPull_RefusesALinkAddedAfterACleanFetch(t *testing.T) {
	f := newPullFixture(t, map[string]string{"clean.md": "a\n"})
	captureLog(t)
	f.peerPush("p1.md", "1\n")
	if err := runPull(f.repo, "main"); err != nil {
		t.Fatalf("the clean fetch must merge: %v", err)
	}
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte(linkSecret), 0o600)
	mustGit(t, f.peer, "pull", "-q", "origin", "main")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(f.peer.Dir, "projects", "p1", "memory", "later.md")); err != nil {
		t.Skip(err)
	}
	mustGit(t, f.peer, "add", "-A")
	mustGit(t, f.peer, "commit", "-q", "-m", "later link")
	mustGit(t, f.peer, "push", "-q", "origin", "main")
	f.loop.pullTick(f.repo, f.roots)
	if err := runPull(f.repo, "main"); !errors.Is(err, ErrLocalOnlyTracked) {
		t.Fatalf("a link in a later commit must be refused, got %v", err)
	}
	if _, err := os.Lstat(filepath.Join(f.repo.Dir, "projects", "p1", "memory", "later.md")); err == nil {
		t.Error("the link reached the work tree")
	}
	if line := PullRefusedLine(f.repo.Dir); !strings.Contains(line, "later.md") || strings.Contains(line, linkSecret) {
		t.Errorf("the hold must name the path only: %q", line)
	}
}
