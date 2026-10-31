package sync

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

func (f *pullFixture) peerForce(rel, content string) {
	f.t.Helper()
	mustGit(f.t, f.peer, "pull", "-q", "origin", "main")
	writeRepoFile(f.t, f.peer, rel, content)
	mustGit(f.t, f.peer, "add", "-f", rel)
	mustGit(f.t, f.peer, "commit", "-q", "-m", "peer: "+rel)
	mustGit(f.t, f.peer, "push", "-q", "origin", "main")
}

func headFiles(t *testing.T, r *gitwt.Repo) string {
	t.Helper()
	out, err := r.Run("ls-tree", "-r", "--name-only", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestLocalOnlyPath(t *testing.T) {
	for p, want := range map[string]bool{
		"config.json":                    true,
		"daemon.pid":                     true,
		"denylist.txt":                   true,
		".state/visibility":              true,
		".state/a/b":                     true,
		"x.tmp":                          true,
		"x.tmp.1":                        true,
		"projects/p/memory/ideas.tmp.md": false,
		"projects/p/memory/plan.tmp":     false,
		"projects/p/memory/a.tmp.1":      false,
		"projects/p/memory/.a.md.1f2e3d4c5b6a7988.tmp":     true,
		"projects/p/memory/n.from-remote-0123456789abcdef": true,
		"projects/p/memory/n.from-remote-abc":              false,
		"projects/p/memory/n.from-remote-notes":            false,
		"distilled/DISTILLED.md":                           true,
		"projects/p/memory/config.json":                    false, // only the root one is the daemon's
		"projects/p/memory/a.md":                           false,
		"notes/denylist.txt":                               false,
		".gitignore":                                       false,
		".statex/a":                                        false,
	} {
		if gitwt.LocalOnlyPath(p) != want {
			t.Errorf("LocalOnlyPath(%q) = %v, want %v", p, !want, want)
		}
	}
}

// A remote commit that tracks config.json must not be merged: the pull is
// refused before the rebase, the local config.json is untouched, sync pauses
// with a status line, and it recovers once the remote tree is clean again.
func TestPull_RefusesTrackedLocalOnlyFiles(t *testing.T) {
	for _, rel := range []string{"config.json", "denylist.txt", ".state/visibility", "daemon.pid"} {
		t.Run(rel, func(t *testing.T) {
			f := newPullFixture(t, map[string]string{"clean.md": "a\n"})
			logs := captureLog(t)
			local := filepath.Join(f.repo.Dir, filepath.FromSlash(rel))
			os.MkdirAll(filepath.Dir(local), 0o700)
			if err := os.WriteFile(local, []byte("LOCAL-"+rel), 0o600); err != nil {
				t.Fatal(err)
			}
			before := f.head()
			f.peerForce(rel, `{"githubTokenCmd":["/bin/evil"]}`)
			// the daemon cycle: no merge, no push, a status, logged once
			f.loop.pullTick(f.repo, f.roots)
			f.editLocal("clean.md", "a\nlocal\n") // a local change keeps the next cycle from idling
			f.loop.pullTick(f.repo, f.roots)
			if f.originHas("projects/p1/memory/clean.md") && f.originFile("clean.md") != "a\n" {
				t.Error("a push went out while the pull was refused")
			}
			if strings.Contains(headFiles(t, f.repo), rel) {
				t.Error("cycle merged the hostile tree")
			}
			if got, _ := os.ReadFile(local); string(got) != "LOCAL-"+rel {
				t.Errorf("local file overwritten by a cycle: %q", got)
			}
			if err := runPull(f.repo, "main"); !errors.Is(err, ErrLocalOnlyTracked) {
				t.Fatalf("pull must be refused, got %v", err)
			}
			if f.head() != before {
				// local commit may have been made by the cycle; HEAD must only differ by it
				if strings.Contains(headFiles(t, f.repo), rel) {
					t.Error("HEAD tracks the hostile file")
				}
			}
			if line := PullRefusedLine(f.repo.Dir); !strings.Contains(line, "pull refused") || !strings.Contains(line, rel) {
				t.Errorf("status line: %q", line)
			}
			if n := strings.Count(logs.String(), "pull refused"); n != 1 {
				t.Errorf("must log once, logged %d times:\n%s", n, logs.String())
			}
			// the remote removes it: sync resumes and the status clears
			mustGit(t, f.peer, "pull", "-q", "origin", "main")
			mustGit(t, f.peer, "rm", "-q", "-f", rel)
			mustGit(t, f.peer, "commit", "-q", "-m", "clean")
			mustGit(t, f.peer, "push", "-q", "origin", "main")
			f.loop.pullTick(f.repo, f.roots)
			if line := PullRefusedLine(f.repo.Dir); line != "" {
				t.Errorf("status must clear: %q", line)
			}
			if got, _ := os.ReadFile(local); string(got) != "LOCAL-"+rel {
				t.Errorf("local file touched: %q", got)
			}
		})
	}
}

func TestPull_OrdinaryRemoteChangesStillPull(t *testing.T) {
	f := newPullFixture(t, map[string]string{"clean.md": "a\n"})
	captureLog(t)
	f.peerPush("p1.md", "1\n")
	if err := runPull(f.repo, "main"); err != nil {
		t.Fatal(err)
	}
	if line := PullRefusedLine(f.repo.Dir); line != "" {
		t.Error(line)
	}
}

// A note named like a temp file is just a note: pulling it works.
func TestPull_NoteNamedLikeATempFileIsNotRefused(t *testing.T) {
	f := newPullFixture(t, map[string]string{"clean.md": "a\n"})
	captureLog(t)
	for _, name := range []string{"ideas.tmp.md", "plan.tmp", "x.tmp.1"} {
		f.peerForce("projects/p1/memory/"+name, "idea\n")
	}
	if err := runPull(f.repo, "main"); err != nil {
		t.Fatalf("a note called ideas.tmp.md must not refuse the pull: %v", err)
	}
	if line := PullRefusedLine(f.repo.Dir); line != "" {
		t.Error(line)
	}
}

// A fetched tree that tracks a per-PC name under another spelling is refused
// just the same: on a case-insensitive file system (simulated with
// core.ignorecase where the file system is case-sensitive) the spelling lands
// on this host's own file. The fetch goes through the daemon's pull cycle, the
// host's untracked, ignored file is unchanged, and nothing is merged.
func TestPull_RefusesCaseVariantLocalOnlyFiles(t *testing.T) {
	for _, c := range []struct{ tracked, local string }{
		{"Config.json", "config.json"},
		{"CONFIG.JSON", "config.json"},
		{"Denylist.txt", "denylist.txt"},
		{".State/visibility", ".state/visibility"},
		{"Daemon.pid", "daemon.pid"},
		{"config.json.", "config.json"},
		{"CONFIG~1.JSO", "config.json"},
		{"config.json/x", "config.json"},
		{"Config.json/x", "config.json"},
		{"Daemon.pid/x", "daemon.pid"},
		{".State", ".state/visibility"},
		{".state", ".state/visibility"},
		{"CO1A2B~1.JSO", "config.json"},
	} {
		t.Run(c.tracked, func(t *testing.T) {
			f := newPullFixture(t, map[string]string{"clean.md": "a\n"})
			captureLog(t)
			mustGit(t, f.repo, "config", "core.ignorecase", "true")
			local := filepath.Join(f.repo.Dir, filepath.FromSlash(c.local))
			os.MkdirAll(filepath.Dir(local), 0o700)
			if err := os.WriteFile(local, []byte("LOCAL-"+c.local), 0o600); err != nil {
				t.Fatal(err)
			}
			f.peerForce(c.tracked, `{"scrub":false,"privateOnly":false}`)

			f.loop.pullTick(f.repo, f.roots)
			f.editLocal("clean.md", "a\nlocal\n")
			f.loop.pullTick(f.repo, f.roots)
			if err := runPull(f.repo, "main"); !errors.Is(err, ErrLocalOnlyTracked) {
				t.Fatalf("pull of %q must be refused, got %v", c.tracked, err)
			}
			if got, _ := os.ReadFile(local); string(got) != "LOCAL-"+c.local {
				t.Errorf("host file changed: %q", got)
			}
			if strings.Contains(strings.ToLower(headFiles(t, f.repo)), strings.ToLower(c.tracked)) {
				t.Errorf("HEAD tracks %q", c.tracked)
			}
			if line := PullRefusedLine(f.repo.Dir); !strings.Contains(line, "pull refused") {
				t.Errorf("status line: %q", line)
			}
		})
	}
}

// A sync dir that cannot be read (for instance a "~/" path that was never
// expanded) must not read as "no remote branch yet": the guard fails closed.
func TestRefuseLocalOnly_FailsClosedOnAnUnreadableDir(t *testing.T) {
	repo := gitwt.New(filepath.Join(t.TempDir(), "~", "missing"))
	err := refuseLocalOnly(repo, "main")
	if err == nil {
		t.Fatal("an unusable repo dir must be an error, not a pass")
	}
}
