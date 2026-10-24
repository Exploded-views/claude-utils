package sync

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/config"
)

// machine is one simulated PC: its own ~/.claude/projects and project
// checkouts, and its own sync work-tree (its .state/ is per-PC). Git carrying
// the repo between PCs is modelled by pullFrom.
type machine struct {
	t      *testing.T
	home   string
	claude string
	cfg    config.Config
}

func newMachine(t *testing.T, name string) *machine {
	t.Helper()
	home := filepath.Join(t.TempDir(), name)
	cfg := config.Defaults()
	cfg.SyncDir = filepath.Join(home, ".claudesync")
	cfg.ClaudeProjectsDir = filepath.Join(home, ".claude", "projects")
	cfg.ProjectKey = config.ProjectKeyRemote
	if err := os.MkdirAll(cfg.ClaudeProjectsDir, 0700); err != nil {
		t.Fatal(err)
	}
	return &machine{t: t, home: home, claude: cfg.ClaudeProjectsDir, cfg: cfg}
}

func (m *machine) hash(projectDir string) string { return EscapeProjectPath(projectDir) }

// open makes the project folder Claude creates when a session starts there
// (transcripts only, no memory/ yet).
func (m *machine) open(projectDir string) {
	m.t.Helper()
	if err := os.MkdirAll(filepath.Join(m.claude, m.hash(projectDir)), 0700); err != nil {
		m.t.Fatal(err)
	}
}

func (m *machine) hasMemDir(projectDir string) bool {
	_, err := os.Stat(filepath.Join(m.claude, m.hash(projectDir), "memory"))
	return err == nil
}

func (m *machine) remove(projectDir, name string) {
	m.t.Helper()
	if err := os.Remove(filepath.Join(m.claude, m.hash(projectDir), "memory", name)); err != nil {
		m.t.Fatal(err)
	}
}

func (m *machine) write(projectDir, name, content string) {
	m.t.Helper()
	writeMem(m.t, m.claude, m.hash(projectDir), name, content)
}

func (m *machine) read(projectDir, name string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(m.claude, m.hash(projectDir), "memory", name))
	return string(b), err == nil
}

func (m *machine) remoteMap() *RemoteMap {
	m.t.Helper()
	rm, err := NewRemoteMap(m.cfg.SyncDir, m.claude)
	if err != nil {
		m.t.Fatal(err)
	}
	return rm
}

// captureLog collects the standard logger's output until the test ends.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

const appKey = "github.com/acme/app"

func TestRemoteMap_KeysAFolderByItsOrigin(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:Acme/App.git")
	plain := mkdirs(t, a.home, "notes")[0]
	noOrigin := mkRepo(t, filepath.Join(a.home, "local-only"), "")
	localRemote := mkRepo(t, filepath.Join(a.home, "localremote"), "/srv/git/x.git")
	for _, d := range []string{app, plain, noOrigin, localRemote} {
		a.write(d, "n.md", "x\n")
	}
	rm := a.remoteMap()
	if got := rm.Key(a.hash(app)); got != appKey {
		t.Errorf("checkout key = %q", got)
	}
	for _, d := range []string{plain, noOrigin, localRemote} {
		if got := rm.Key(a.hash(d)); got != "" {
			t.Errorf("%s should keep its path key, got %q", filepath.Base(d), got)
		}
	}
	if got, want := rm.MemoryDir(appKey), filepath.Join(a.cfg.SyncDir, "remotes", "github.com", "acme", "app", "memory"); got != want {
		t.Errorf("MemoryDir = %q, want %q", got, want)
	}
}

func TestRemoteMap_SubdirectoryAndLinkedWorktreeKeepPathKeys(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	main := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	sub := mkdirs(t, main, "pkg")[0]
	wt := filepath.Join(a.home, "src", "app-feature")
	addWorktree(t, main, wt)
	for _, d := range []string{main, sub, wt} {
		a.open(d)
	}
	rm := a.remoteMap()
	if got := rm.Key(a.hash(main)); got != appKey {
		t.Errorf("main checkout key = %q", got)
	}
	for name, d := range map[string]string{"subdirectory": sub, "linked worktree": wt} {
		if got := rm.Key(a.hash(d)); got != "" {
			t.Errorf("%s must be path-keyed, got %q", name, got)
		}
	}
}

// At most one folder per PC holds a remote key. The first to claim it keeps
// it; another folder resolving to the same key falls back to its path key and
// the daemon says so once.
func TestRemoteMap_SecondCloneOfTheSameRemoteFallsBackToItsPathKey(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	first := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	second := mkRepo(t, filepath.Join(a.home, "src", "app-copy"), "https://github.com/acme/app.git")
	a.open(first)
	a.open(second)
	logs := captureLog(t)

	rm := a.remoteMap()
	rm.Refresh()
	if got := rm.Key(a.hash(first)); got != appKey {
		t.Errorf("first folder key = %q, want %q", got, appKey)
	}
	if got := rm.Key(a.hash(second)); got != "" {
		t.Errorf("second clone key = %q, want the path key", got)
	}
	out := logs.String()
	if n := strings.Count(out, a.hash(second)); n != 1 {
		t.Errorf("want one log line naming the second clone, got %d in %q", n, out)
	}
	if strings.Contains(out, a.home) {
		t.Errorf("the log leaks a path: %q", out)
	}
}

// The claim is sticky: it survives a restart even when sort order would now
// favour the other folder, and is released only by positive evidence.
func TestRemoteMap_TheFirstClaimIsStickyAcrossRestarts(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	late := mkRepo(t, filepath.Join(a.home, "src", "zeta"), "git@github.com:acme/app.git")
	a.open(late)
	rm := a.remoteMap()
	if got := rm.Key(a.hash(late)); got != appKey {
		t.Fatalf("zeta key = %q", got)
	}
	// A clone that sorts first appears afterwards.
	early := mkRepo(t, filepath.Join(a.home, "src", "alpha"), "git@github.com:acme/app.git")
	a.open(early)

	rm2 := a.remoteMap()
	if got := rm2.Key(a.hash(late)); got != appKey {
		t.Errorf("the original owner lost its key after a restart: %q", got)
	}
	if got := rm2.Key(a.hash(early)); got != "" {
		t.Errorf("the newcomer took the key: %q", got)
	}

	// The owner's checkout going away does not hand the key to the newcomer.
	if err := os.RemoveAll(late); err != nil {
		t.Fatal(err)
	}
	rm3 := a.remoteMap()
	if got := rm3.Key(a.hash(late)); got != appKey {
		t.Errorf("deleted checkout lost its key: %q", got)
	}
	if got := rm3.Key(a.hash(early)); got != "" {
		t.Errorf("newcomer took the key from a deleted checkout: %q", got)
	}

	// Positive evidence (git says another origin) releases it.
	rm3.Lookup = func(dir string) (string, bool) {
		if dir == early {
			return "git@github.com:acme/app.git", true
		}
		return "git@github.com:acme/moved.git", true
	}
	// The owner's checkout is back, now pointing elsewhere.
	mkdirs(t, a.home, "src/zeta")
	rm3.Refresh()
	rm3.Refresh() // a key released during a pass is taken by the next one
	if got := rm3.Key(a.hash(late)); got != "github.com/acme/moved" {
		t.Errorf("owner = %q after its origin changed", got)
	}
	if got := rm3.Key(a.hash(early)); got != appKey {
		t.Errorf("released key not taken by the other clone: %q", got)
	}
}

func TestRemoteMap_AnOverrideCannotJoinAKeyAnotherFolderHolds(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	fork := mkRepo(t, filepath.Join(a.home, "src", "fork"), "git@github.com:me/fork.git")
	a.open(app)
	a.open(fork)
	c := &ProjectCache{Projects: map[string]ProjectEntry{
		a.hash(fork): {Key: appKey}, // would sort-wise come first
	}}
	if err := c.Save(a.cfg.SyncDir); err != nil {
		t.Fatal(err)
	}
	rm := a.remoteMap()
	owners := 0
	for _, d := range []string{app, fork} {
		if rm.Key(a.hash(d)) == appKey {
			owners++
		}
	}
	if owners != 1 {
		t.Errorf("%d folders hold %s, want exactly one", owners, appKey)
	}
}

func TestRemoteMap_AmbiguousDecodeFallsBackToPathKeyAndRecordsCandidates(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	dashed := mkRepo(t, filepath.Join(a.home, "src", "my-app"), "git@github.com:acme/one.git")
	mkRepo(t, filepath.Join(a.home, "src", "my.app"), "git@github.com:acme/two.git")
	hash := a.hash(dashed)
	a.write(dashed, "note.md", "hi\n")

	rm := a.remoteMap()
	if got := rm.Key(hash); got != "" {
		t.Errorf("ambiguous folder key = %q, want the path key", got)
	}
	c, err := LoadProjectCache(a.cfg.SyncDir)
	if err != nil {
		t.Fatal(err)
	}
	e := c.Projects[hash]
	if e.Dir != "" || len(e.Candidates) != 2 {
		t.Errorf("want 2 recorded candidates and no dir, got %+v", e)
	}

	// The user pins the right directory by hand; the next run uses it.
	e.Dir = dashed
	c.Projects[hash] = e
	if err := c.Save(a.cfg.SyncDir); err != nil {
		t.Fatal(err)
	}
	if got := a.remoteMap().Key(hash); got != "github.com/acme/one" {
		t.Errorf("pinned dir: key = %q", got)
	}
}

// The log lines about one folder name the Claude folder and keys only: a
// later change redacts them in one place, and a path would be a leak there.
func TestRemoteMap_FolderLogLinesCarryNoPaths(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	dashed := mkRepo(t, filepath.Join(a.home, "src", "my-app"), "git@github.com:acme/one.git")
	mkRepo(t, filepath.Join(a.home, "src", "my.app"), "git@github.com:acme/two.git")
	logs := captureLog(t)
	a.write(dashed, "note.md", "hi\n")
	a.remoteMap()
	out := logs.String()
	if !strings.Contains(out, a.hash(dashed)) || !strings.Contains(out, "matches 2 directories") {
		t.Fatalf("want the candidates line for the folder, got %q", out)
	}
	if strings.Contains(out, a.cfg.SyncDir) || strings.Contains(out, a.home) {
		t.Errorf("the candidates line leaks a path: %q", out)
	}
}

// A cache that could not be saved is still dirty: the next save retries it.
func TestRemoteMap_FailedSaveIsRetried(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	a.open(app)
	m := a.remoteMap()
	// Forget the saved cache, then make .state/ unusable so the save fails.
	if err := os.RemoveAll(filepath.Join(a.cfg.SyncDir, ".state")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.cfg.SyncDir, ".state"), []byte("in the way"), 0600); err != nil {
		t.Fatal(err)
	}
	logs := captureLog(t)
	m.cache.Projects = map[string]ProjectEntry{}
	m.Refresh()
	if !strings.Contains(logs.String(), "save cache") {
		t.Fatalf("setup: the save should have failed, log = %q", logs.String())
	}
	if err := os.Remove(filepath.Join(a.cfg.SyncDir, ".state")); err != nil {
		t.Fatal(err)
	}
	m.save()
	if _, err := os.Stat(ProjectCachePath(a.cfg.SyncDir)); err != nil {
		t.Errorf("the failed save was not retried: %v", err)
	}
}

// A claim is released only when nothing is left of its holder: the Claude
// folder, the recorded checkout and any directory the name decodes to.
func TestRemoteMap_ClaimIsReleasedOnlyWhenEverythingOfTheHolderIsGone(t *testing.T) {
	gitEnv(t)
	for _, c := range []struct {
		name         string
		keepFolder   bool // the Claude project folder still exists
		keepCheckout bool // the recorded checkout still exists
		keepDecoded  bool // another directory escapes to the same name
		released     bool
	}{
		{"everything gone", false, false, false, true},
		{"claude folder still there", true, false, false, false},
		{"checkout still there", false, true, false, false},
		{"name still decodes to a directory", false, false, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := newMachine(t, "alice")
			old := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
			a.write(old, "n.md", "1\n")
			a.remoteMap() // old claims the key and records its dir
			if c.keepDecoded {
				// The recorded dir is gone but the name still decodes to old.
				e := ProjectEntry{Dir: filepath.Join(a.home, "elsewhere", "app"), LastKey: appKey}
				ca, _ := LoadProjectCache(a.cfg.SyncDir)
				ca.Projects[a.hash(old)] = e
				if err := ca.Save(a.cfg.SyncDir); err != nil {
					t.Fatal(err)
				}
			}
			if !c.keepFolder {
				if err := os.RemoveAll(filepath.Join(a.claude, a.hash(old))); err != nil {
					t.Fatal(err)
				}
			}
			if !c.keepCheckout && !c.keepDecoded {
				if err := os.RemoveAll(old); err != nil {
					t.Fatal(err)
				}
			}

			fresh := mkRepo(t, filepath.Join(a.home, "work", "app"), "git@github.com:acme/app.git")
			a.open(fresh)
			captureLog(t)
			rm := a.remoteMap()
			got := rm.Key(a.hash(fresh))
			if c.released && got != appKey {
				t.Errorf("fresh clone key = %q, want it to take over %q", got, appKey)
			}
			if !c.released && got != "" {
				t.Errorf("fresh clone key = %q, want its path key: the claim must not move by itself", got)
			}
			ca, _ := LoadProjectCache(a.cfg.SyncDir)
			_, kept := ca.Projects[a.hash(old)]
			if kept == c.released {
				t.Errorf("old holder's cache entry kept = %v, released = %v", kept, c.released)
			}
		})
	}
}

// When the holder's Claude folder exists but its checkout is gone, the refusal
// says so and gives the edit that moves the claim by hand.
func TestRemoteMap_RefusalSaysTheHoldersCheckoutIsGoneAndHowToMoveTheClaim(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	old := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	a.write(old, "n.md", "1\n")
	a.remoteMap()
	if err := os.RemoveAll(old); err != nil {
		t.Fatal(err)
	}
	fresh := mkRepo(t, filepath.Join(a.home, "work", "app"), "git@github.com:acme/app.git")
	a.open(fresh)
	logs := captureLog(t)
	a.remoteMap()
	out := logs.String()
	for _, want := range []string{a.hash(fresh), a.hash(old), "checkout is gone", ".state/projects.json",
		`"key": "path"`, `"key": "` + appKey + `"`, "stop the daemon"} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal log lacks %q: %q", want, out)
		}
	}
	if strings.Contains(out, a.home) {
		t.Errorf("the log leaks a path: %q", out)
	}
}

// The same hint when the holder's checkout is still there but its Claude
// folder is gone: the claim does not move by itself then either.
func TestRemoteMap_RefusalSaysTheHoldersClaudeFolderIsGoneAndHowToMoveTheClaim(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	old := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	a.write(old, "n.md", "1\n")
	a.remoteMap()
	if err := os.RemoveAll(filepath.Join(a.claude, a.hash(old))); err != nil {
		t.Fatal(err)
	}
	fresh := mkRepo(t, filepath.Join(a.home, "work", "app"), "git@github.com:acme/app.git")
	a.open(fresh)
	logs := captureLog(t)
	a.remoteMap()
	out := logs.String()
	for _, want := range []string{a.hash(fresh), a.hash(old), "Claude folder is gone", ".state/projects.json",
		`"key": "path"`, `"key": "` + appKey + `"`, "stop the daemon"} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal log lacks %q: %q", want, out)
		}
	}
	if strings.Contains(out, a.home) {
		t.Errorf("the log leaks a path: %q", out)
	}
}

// Every key change is logged with the folder and both keys.
func TestRemoteMap_KeyChangeIsLogged(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	a.write(app, "n.md", "1\n")
	hash := a.hash(app)
	logs := captureLog(t)
	a.remoteMap() // path -> remote
	setCacheKey(t, a, app, "path")
	a.remoteMap() // remote -> path
	out := logs.String()
	for _, want := range []string{hash + ": key changed from path to " + appKey, hash + ": key changed from " + appKey + " to path"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q: %q", want, out)
		}
	}
	if strings.Contains(out, a.home) {
		t.Errorf("the log leaks a path: %q", out)
	}
}

func TestRemoteMap_AmbiguousButSameRemoteResolves(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	d1 := mkRepo(t, filepath.Join(a.home, "src", "my-app"), "git@github.com:acme/app.git")
	mkRepo(t, filepath.Join(a.home, "src", "my.app"), "https://github.com/acme/app.git")
	a.write(d1, "note.md", "hi\n")
	if got := a.remoteMap().Key(a.hash(d1)); got != appKey {
		t.Errorf("candidates that agree on the remote are not ambiguous: %q", got)
	}
}

func TestRemoteMap_UnresolvableKeepsPathKey(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	writeMem(t, a.claude, "-gone-away-project", "note.md", "hi\n")
	if got := a.remoteMap().Key("-gone-away-project"); got != "" {
		t.Errorf("key = %q", got)
	}
}

func TestRemoteMap_CacheKeyOverrideWins(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	other := mkRepo(t, filepath.Join(a.home, "src", "fork"), "git@github.com:me/fork.git")
	a.open(app)
	a.open(other)
	c := &ProjectCache{Projects: map[string]ProjectEntry{
		a.hash(app):   {Key: "path"}, // keep per-path on purpose
		a.hash(other): {Key: appKey}, // fold the fork into upstream's folder
	}}
	if err := c.Save(a.cfg.SyncDir); err != nil {
		t.Fatal(err)
	}
	rm := a.remoteMap()
	if got := rm.Key(a.hash(app)); got != "" {
		t.Errorf("key \"path\" should force the path key, got %q", got)
	}
	if got := rm.Key(a.hash(other)); got != appKey {
		t.Errorf("explicit key should be used verbatim, got %q", got)
	}
}

func TestRemoteMap_InvalidKeyOverrideIsIgnoredAndLogged(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	a.open(app)
	c := &ProjectCache{Projects: map[string]ProjectEntry{a.hash(app): {Key: "../../escaped"}}}
	if err := c.Save(a.cfg.SyncDir); err != nil {
		t.Fatal(err)
	}
	logs := captureLog(t)
	rm := a.remoteMap()
	rm.Refresh()
	if got := rm.Key(a.hash(app)); got != appKey {
		t.Errorf("an invalid key should fall back to the derived remote key, got %q", got)
	}
	if n := strings.Count(logs.String(), "../../escaped"); n != 1 {
		t.Errorf("rejected override should be logged once, got %d in %q", n, logs.String())
	}
	// Left in the file for the user to fix.
	c2, err := LoadProjectCache(a.cfg.SyncDir)
	if err != nil || c2.Projects[a.hash(app)].Key != "../../escaped" {
		t.Errorf("the override was not preserved: %+v %v", c2.Projects, err)
	}
}

// Deleting a checkout must not flip its key back to the path key.
func TestRemoteMap_DeletedCheckoutKeepsItsKey(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	a.write(app, "a.md", "a\n")
	if got := a.remoteMap().Key(a.hash(app)); got != appKey {
		t.Fatalf("initial key = %q", got)
	}
	if err := os.RemoveAll(app); err != nil {
		t.Fatal(err)
	}
	if got := a.remoteMap().Key(a.hash(app)); got != appKey {
		t.Errorf("deleted checkout re-keyed to %q", got)
	}
}

// Only positive evidence re-keys a folder: git ran and said there is no origin
// (or a different one). Anything that stops git answering keeps the last key.
func TestRemoteMap_StickyKeyOnlyChangesOnPositiveEvidence(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	a.write(app, "a.md", "a\n")
	hash := a.hash(app)

	m := a.remoteMap()
	if got := m.Key(hash); got != appKey {
		t.Fatalf("initial key = %q", got)
	}
	steps := []struct {
		name string
		url  string
		ok   bool
		want string
	}{
		{"git error (dubious ownership, not installed, ...)", "", false, appKey},
		{"git error again", "", false, appKey},
		{"different origin", "git@github.com:acme/other.git", true, "github.com/acme/other"},
		{"git error after the change", "", false, "github.com/acme/other"},
		{"git says there is no origin", "", true, ""},
	}
	for _, st := range steps {
		m.Lookup = func(string) (string, bool) { return st.url, st.ok }
		m.Refresh()
		if got := m.Key(hash); got != st.want {
			t.Errorf("%s: key = %q, want %q", st.name, got, st.want)
		}
	}

	// The last key survives a restart: it lives in the cache, not in memory.
	m.Lookup = func(string) (string, bool) { return "git@github.com:acme/app.git", true }
	m.Refresh()
	m2 := a.remoteMap()
	m2.Lookup = func(string) (string, bool) { return "", false }
	m2.Refresh()
	if got := m2.Key(hash); got != appKey {
		t.Errorf("after restart with git failing, key = %q", got)
	}
}

// The recorded directory is gone and the only directory the name decodes to now
// is a different one. Having no origin says nothing about the project that used
// to be there, so only a real origin may re-key the folder.
func TestRemoteMap_StickyKeyIgnoresAForeignCandidateWithoutOrigin(t *testing.T) {
	for _, tc := range []struct {
		name   string
		origin string // of the new candidate; "" means a plain directory
		want   string
		dirNow bool // the cache now records the new candidate
	}{
		{"plain directory", "", appKey, false},
		{"checkout without origin", "-", appKey, false},
		{"checkout with another origin", "git@github.com:acme/other.git", "github.com/acme/other", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gitEnv(t)
			a := newMachine(t, "alice")
			old := mkRepo(t, filepath.Join(a.home, "src", "my-app"), "git@github.com:acme/app.git")
			hash := a.hash(old)
			a.write(old, "a.md", "a\n")
			m := a.remoteMap()
			if got := m.Key(hash); got != appKey {
				t.Fatalf("initial key = %q", got)
			}
			if err := os.RemoveAll(old); err != nil {
				t.Fatal(err)
			}
			repl := filepath.Join(a.home, "src", "my.app")
			switch tc.origin {
			case "":
				mkdirs(t, a.home, "src/my.app")
			case "-":
				mkRepo(t, repl, "")
			default:
				mkRepo(t, repl, tc.origin)
			}
			m.Refresh()
			if got := m.Key(hash); got != tc.want {
				t.Errorf("key = %q, want %q", got, tc.want)
			}
			c, err := LoadProjectCache(a.cfg.SyncDir)
			if err != nil {
				t.Fatal(err)
			}
			wantDir := old
			if tc.dirNow {
				wantDir = repl
			}
			if c.Projects[hash].Dir != wantDir {
				t.Errorf("cached dir = %q, want %q", c.Projects[hash].Dir, wantDir)
			}
		})
	}
}

// A recorded dir that is missing is kept through a spell where its name decodes
// to several directories: clearing it would throw away a hand-pinned dir.
func TestRemoteMap_AmbiguousDecodeKeepsTheRecordedDir(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	old := mkRepo(t, filepath.Join(a.home, "src", "my-app"), "git@github.com:acme/one.git")
	hash := a.hash(old)
	a.write(old, "a.md", "a\n")
	m := a.remoteMap()
	if err := os.RemoveAll(old); err != nil {
		t.Fatal(err)
	}
	mkRepo(t, filepath.Join(a.home, "src", "my.app"), "git@github.com:acme/two.git")
	mkRepo(t, filepath.Join(a.home, "src", "my", "app"), "git@github.com:acme/three.git")
	m.Refresh()
	if got := m.Key(hash); got != "github.com/acme/one" {
		t.Errorf("key = %q, want the remembered one", got)
	}
	c, err := LoadProjectCache(a.cfg.SyncDir)
	if err != nil {
		t.Fatal(err)
	}
	e := c.Projects[hash]
	if e.Dir != old || len(e.Candidates) != 2 {
		t.Errorf("want the old dir kept and 2 candidates, got %+v", e)
	}
}

// When git cannot answer and there is no remembered key, say so once per
// folder, naming the Claude folder and the class of failure but not the path.
func TestRemoteMap_LogsOnceWhenGitCannotAnswerAndNothingIsRemembered(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	a.write(app, "a.md", "a\n")
	hash := a.hash(app)

	logs := captureLog(t)
	m := a.remoteMap()
	// A cache that knows the directory but no key, as after a first run where
	// git was already failing.
	m.cache = &ProjectCache{Version: 1, Projects: map[string]ProjectEntry{hash: {Dir: app}}}
	m.Lookup = func(string) (string, bool) { return "", false }
	m.Why = func(string) string { return "dubious ownership" }
	logs.Reset()
	m.Refresh()
	m.Refresh()
	out := logs.String()
	if n := strings.Count(out, hash); n != 1 {
		t.Fatalf("want one log line naming %s, got %d in %q", hash, n, out)
	}
	if !strings.Contains(out, "dubious ownership") {
		t.Errorf("the reason class is missing: %q", out)
	}
	if strings.Contains(out, a.home) {
		t.Errorf("the log leaks the project path: %q", out)
	}

	// With a remembered key there is nothing to complain about.
	m.cache.Projects[hash] = ProjectEntry{Dir: app, LastKey: appKey}
	m.warned = map[string]bool{}
	m.failed = map[string]bool{}
	logs.Reset()
	m.Refresh()
	if logs.Len() != 0 {
		t.Errorf("logged although the key is remembered: %q", logs.String())
	}
}

// A projects/<hash> folder that came from another PC's push has no project
// folder here: it must not be resolved or cached.
func TestRemoteMap_ForeignHashIsNotResolvedOrCached(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	a.write(app, "a.md", "a\n")
	m := a.remoteMap()
	m.Lookup = func(dir string) (string, bool) {
		t.Errorf("Lookup(%q) for a folder that is not under this PC's projects", dir)
		return "", false
	}
	if got := m.Key("-home-bob-src-app"); got != "" {
		t.Errorf("Key = %q, want the path key", got)
	}
	c, err := LoadProjectCache(a.cfg.SyncDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Projects["-home-bob-src-app"]; ok {
		t.Error("foreign hash written to the cache")
	}
}

func TestRemoteMap_CacheRecordsResolvedDirUnderState(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	a.write(app, "a.md", "a\n")
	a.remoteMap()
	c, err := LoadProjectCache(a.cfg.SyncDir)
	if err != nil {
		t.Fatal(err)
	}
	if e := c.Projects[a.hash(app)]; e.Dir != app || e.LastKey != appKey {
		t.Errorf("cache should record the resolved dir and key, got %+v", e)
	}
	if !strings.Contains(filepath.ToSlash(ProjectCachePath(a.cfg.SyncDir)), "/.state/") {
		t.Errorf("cache path %s should be under .state/", ProjectCachePath(a.cfg.SyncDir))
	}
	// Written atomically: nothing but the file itself is left behind.
	entries, _ := os.ReadDir(filepath.Dir(ProjectCachePath(a.cfg.SyncDir)))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file %s", e.Name())
		}
	}
}

// A corrupt (hand-edited or torn) projects.json must not stop the daemon: it is
// set aside and resolution starts from an empty cache.
func TestRemoteMap_CorruptCacheIsSetAsideAndTheDaemonCarriesOn(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	p := ProjectCachePath(a.cfg.SyncDir)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	const torn = `{"version":1,"projects":{`
	if err := os.WriteFile(p, []byte(torn), 0600); err != nil {
		t.Fatal(err)
	}
	app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	a.write(app, "a.md", "a\n")

	m := a.remoteMap() // fails the test on error
	if got := m.Key(a.hash(app)); got != appKey {
		t.Errorf("keys still resolve with an empty cache: %q", got)
	}
	matches, _ := filepath.Glob(p + ".corrupt-*")
	if len(matches) != 1 {
		t.Fatalf("want the corrupt file renamed aside, got %v", matches)
	}
	if b, _ := os.ReadFile(matches[0]); string(b) != torn {
		t.Errorf("set-aside content = %q", b)
	}
	c, err := LoadProjectCache(a.cfg.SyncDir)
	if err != nil || c.Projects[a.hash(app)].Dir != app {
		t.Errorf("fresh cache should have been written: %+v, %v", c, err)
	}
}

func TestRemoteMap_UnreadableCacheDoesNotStopTheDaemon(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	app := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	a.write(app, "a.md", "a\n")
	if err := os.MkdirAll(filepath.Join(ProjectCachePath(a.cfg.SyncDir), "dir"), 0700); err != nil {
		t.Fatal(err)
	}
	if got := a.remoteMap().Key(a.hash(app)); got != appKey {
		t.Errorf("keys still resolve without the cache file: %q", got)
	}
}

// A hand-edited cache saved with a UTF-8 BOM (PowerShell 5 Set-Content
// -Encoding UTF8, older Notepad) is valid JSON to the user and must be read,
// not set aside.
func TestLoadProjectCache_StripsUTF8BOM(t *testing.T) {
	dir := t.TempDir()
	p := ProjectCachePath(dir)
	_ = os.MkdirAll(filepath.Dir(p), 0700)
	if err := os.WriteFile(p, []byte("\xef\xbb\xbf{\"version\":1,\"projects\":{\"-x\":{\"key\":\"path\"}}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadProjectCache(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Projects["-x"].Key != "path" {
		t.Errorf("hand edit with a BOM discarded; cache = %+v", c.Projects)
	}
}

func TestLoadProjectCache_UnknownVersionIsSetAside(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		aside      bool
	}{
		{"newer version", `{"version":2,"projects":{"-x":{"key":"path"}}}`, true},
		{"negative version", `{"version":-1,"projects":{}}`, true},
		{"current version", `{"version":1,"projects":{"-x":{"key":"path"}}}`, false},
		// A hand-written file is unlikely to carry the field.
		{"no version", `{"projects":{"-x":{"key":"path"}}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := ProjectCachePath(dir)
			if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			c, err := LoadProjectCache(dir)
			if err != nil {
				t.Fatal(err)
			}
			matches, _ := filepath.Glob(p + ".corrupt-*")
			if tc.aside {
				if len(matches) != 1 || len(c.Projects) != 0 {
					t.Errorf("want the file set aside and an empty cache: %v %v", matches, c.Projects)
				}
				return
			}
			if len(matches) != 0 || c.Projects["-x"].Key != "path" {
				t.Errorf("want the file used: %v %v", matches, c.Projects)
			}
		})
	}
}

// A cache that cannot be read for a reason other than not existing (a
// directory in its place, no permission) is not the daemon's to replace: it
// carries on with an empty in-memory cache and never writes the file.
func TestLoadProjectCache_UnreadableIsNotFatalAndNotOverwritten(t *testing.T) {
	logs := captureLog(t)

	t.Run("directory in place of the file", func(t *testing.T) {
		dir := t.TempDir()
		p := ProjectCachePath(dir)
		if err := os.MkdirAll(filepath.Join(p, "keep"), 0700); err != nil {
			t.Fatal(err)
		}
		c, err := LoadProjectCache(dir)
		if err != nil {
			t.Fatalf("a read error must not be fatal: %v", err)
		}
		c.Projects["-x"] = ProjectEntry{Key: "path"}
		if err := c.Save(dir); err != nil {
			t.Errorf("Save = %v, want a quiet no-op", err)
		}
		if info, err := os.Stat(p); err != nil || !info.IsDir() {
			t.Errorf("the unreadable path was replaced: %v %v", info, err)
		}
	})

	t.Run("permission denied", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Getuid() == 0 {
			t.Skip("cannot make a file unreadable here")
		}
		dir := t.TempDir()
		p := ProjectCachePath(dir)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		const body = `{"version":1,"projects":{"-x":{"key":"path"}}}`
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0); err != nil {
			t.Fatal(err)
		}
		logs.Reset()
		c, err := LoadProjectCache(dir)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(logs.String(), "projects.json") {
			t.Errorf("the read error should be logged: %q", logs.String())
		}
		if err := c.Save(dir); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0600); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(p); string(b) != body {
			t.Errorf("the file was overwritten: %q", b)
		}
	})
}

func TestLoadProjectCache_MissingIsEmpty(t *testing.T) {
	c, err := LoadProjectCache(t.TempDir())
	if err != nil || len(c.Projects) != 0 {
		t.Errorf("got %+v, %v", c, err)
	}
}

// Once the holder's checkout and Claude folder are both gone, a re-clone
// elsewhere gets the key rather than its path key forever.
func TestRemoteSync_StaleClaimIsReleasedWhenHolderIsGone(t *testing.T) {
	gitEnv(t)
	a := newMachine(t, "alice")
	old := mkRepo(t, filepath.Join(a.home, "src", "app"), "git@github.com:acme/app.git")
	a.write(old, "n.md", "1\n")
	a.sync()
	_ = os.RemoveAll(old)
	_ = os.RemoveAll(filepath.Join(a.claude, a.hash(old)))

	fresh := mkRepo(t, filepath.Join(a.home, "work", "app"), "git@github.com:acme/app.git")
	a.open(fresh)
	if got := a.remoteMap().Key(a.hash(fresh)); got != appKey {
		t.Errorf("re-clone key = %q, want %q", got, appKey)
	}
}
