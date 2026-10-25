package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/MarimerLLC/claude-utils/internal/agecrypt"
	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/lock"
	"github.com/MarimerLLC/claude-utils/internal/testenv"
	"github.com/kardianos/service"
)

// encryptRouted turns the plain repo routedHost made for cfg into an
// age-encrypted one (with a local bare origin), and returns its config.
func encryptRouted(t *testing.T, cfg config.Config) config.Config {
	t.Helper()
	origin := filepath.Join(t.TempDir(), "o.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", "-b", "main", origin).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	ms, mm := testenv.Binaries(t)
	if err := os.RemoveAll(cfg.SyncDir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.ClaudeProjectsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg.RemoteURL, cfg.MergeDriverPath = origin, mm
	enc := &encryptSpec{IdentityPath: filepath.Join(t.TempDir(), "k", "identity.txt"), MemsyncExe: ms}
	if err := bootstrapWith(cfg, false, nil, enc); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func newAgeKey(t *testing.T) string {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id.Recipient().String()
}

func commitCount(t *testing.T, dir string) string {
	t.Helper()
	out, err := gitwt.New(dir).Run("rev-list", "--count", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out)
}

func runRcpt(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := recipientsCmd(&out, &errb, args)
	return code, out.String(), errb.String()
}

func TestRoutedRecipients_RefusesWithoutRepoAndListsTheRepos(t *testing.T) {
	cfgs := routedHost(t, []string{"personal", "projects"}, "personal", nil)
	before := commitCount(t, cfgs["personal"].SyncDir)
	code, _, errs := runRcpt("add", newAgeKey(t))
	if code == 0 {
		t.Fatal("recipients acted without --repo in routed mode")
	}
	for _, n := range []string{"personal", "projects", "--repo"} {
		if !strings.Contains(errs, n) {
			t.Errorf("message must mention %q: %s", n, errs)
		}
	}
	if commitCount(t, cfgs["personal"].SyncDir) != before {
		t.Error("the personal repo was touched")
	}
}

func TestRoutedRecipients_RepoActsOnThatRepoAndLocksPerRepo(t *testing.T) {
	cfgs := routedHost(t, []string{"personal", "projects"}, "personal", nil)
	pers := encryptRouted(t, cfgs["personal"])
	proj := encryptRouted(t, cfgs["projects"])
	key := newAgeKey(t)
	persBefore := commitCount(t, pers.SyncDir)

	// The daemon holds personal's lock only: projects is still ours to change.
	held, err := lock.Acquire(pers.SyncDir)
	if err != nil {
		t.Fatal(err)
	}
	if code, _, errs := runRcpt("--repo", "personal", "add", key); code == 0 || !strings.Contains(errs, "daemon") {
		t.Fatalf("personal under a held lock: exit %d %q", code, errs)
	}
	if code, _, errs := runRcpt("--repo", "projects", "add", key); code != 0 {
		t.Fatalf("projects: exit %d %s", code, errs)
	}
	held.Release()

	_, out, _ := runRcpt("--repo", "projects", "list")
	if !strings.Contains(out, key) {
		t.Errorf("projects lacks the key:\n%s", out)
	}
	_, out, _ = runRcpt("list", "--repo", "personal")
	if strings.Contains(out, key) {
		t.Errorf("personal got the key:\n%s", out)
	}
	if commitCount(t, pers.SyncDir) != persBefore {
		t.Error("personal gained a commit")
	}
	// --config names a repo too.
	if code, _, errs := runRcpt("--config", proj.Path(), "remove", key); code != 0 {
		t.Fatalf("--config: exit %d %s", code, errs)
	}
	if code, _, errs := runRcpt("--repo", "nope", "list"); code == 0 || !strings.Contains(errs, "projects") {
		t.Errorf("unknown repo: exit %d %q", code, errs)
	}
	if code, _, _ := runRcpt("--repo", "projects", "--config", proj.Path(), "list"); code == 0 {
		t.Error("--repo with --config must be refused")
	}
}

func TestRoutedDistill_RepoFlagTargetsThatRepoDefaultStaysPersonal(t *testing.T) {
	cfgs := routedHost(t, []string{"personal", "projects"}, "personal", nil)
	idx := func(n string) string { return filepath.Join(cfgs[n].SyncDir, "distilled", "DISTILLED.md") }
	if code, out := captureStdout(t, func() int { return runDistill(nil) }); code != 0 {
		t.Fatalf("default: %d %s", code, out)
	}
	if !fileExists(idx("personal")) || fileExists(idx("projects")) {
		t.Fatal("default distill must write the personal repo only")
	}
	// The catalog lives in the personal repo only.
	if code, _ := captureStdout(t, func() int { return runDistill([]string{"--repo", "projects"}) }); code == 0 || fileExists(idx("projects")) {
		t.Fatal("--repo of a non-personal repo must be refused and write nothing")
	}
	if code, _ := captureStdout(t, func() int { return runDistill([]string{"--config", cfgs["projects"].Path()}) }); code == 0 || fileExists(idx("projects")) {
		t.Fatal("--config of a non-personal repo must be refused too")
	}
	os.Remove(idx("personal"))
	if code, out := captureStdout(t, func() int { return runDistill([]string{"--repo", "personal"}) }); code != 0 || !fileExists(idx("personal")) {
		t.Fatalf("--repo personal: %d %s", code, out)
	}
	if code, _ := captureStdout(t, func() int { return runDistill([]string{"--repo", "nope"}) }); code == 0 {
		t.Error("unknown repo must fail")
	}
	if code, _ := captureStdout(t, func() int { return runDistill([]string{"--repo", "projects", "--config", cfgs["personal"].Path()}) }); code == 0 {
		t.Error("--repo with --config must be refused")
	}
	// Pruning rewrites synced files: it takes that repo's lock, and only that repo's.
	held, err := lock.Acquire(cfgs["personal"].SyncDir)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	if code, _ := captureStdout(t, func() int { return runDistill([]string{"--prune"}) }); code == 0 {
		t.Error("prune under a held lock must be refused")
	}
	if code, _ := captureStdout(t, func() int { return runDistill([]string{"--dry-run"}) }); code != 0 {
		t.Error("a dry run needs no lock")
	}
}

func TestRoutedArchive_LabelsEachRepoAndRepoFlagNarrows(t *testing.T) {
	cfgs := routedHost(t, []string{"personal", "projects"}, "personal", nil)
	for n, body := range map[string]string{"personal": "needle p\n", "projects": "needle q\n"} {
		p := filepath.Join(cfgs[n].SyncDir, "archive", "2026-10-20", "f", n+".md")
		os.MkdirAll(filepath.Dir(p), 0o700)
		os.WriteFile(p, []byte(body), 0o600)
	}
	run := func(args ...string) string {
		var out, errb bytes.Buffer
		if code := archiveCmd(&out, &errb, args); code != 0 {
			t.Fatalf("%v: exit %d %s", args, code, errb.String())
		}
		return out.String()
	}
	all := run("list")
	if !strings.Contains(all, "[personal] ") || !strings.Contains(all, "[projects] ") {
		t.Errorf("both repos, labelled:\n%s", all)
	}
	one := run("search", "needle", "--repo", "projects")
	if !strings.Contains(one, "[projects] ") || strings.Contains(one, "personal") {
		t.Errorf("--repo projects:\n%s", one)
	}
	var out, errb bytes.Buffer
	if code := archiveCmd(&out, &errb, []string{"list", "--repo", "nope"}); code == 0 {
		t.Error("unknown repo must fail")
	}
}

func TestDoctor_TokenFileFlagAndConfig(t *testing.T) {
	const tok = "tok-from-flag-file"
	seen := ""
	cfg := guardCfg(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		orgList(nil)(w, r)
	}, false)
	cfg.RemoteURL = ""
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	tf := filepath.Join(t.TempDir(), "tok")
	os.WriteFile(tf, []byte(tok+"\n"), 0o600)
	var out, errb bytes.Buffer
	if code := doctorCmd(&out, &errb, []string{"--config", cfg.Path(), "--org", "Org"}); code == 0 {
		t.Fatal("without a token doctor must fail")
	}
	out.Reset()
	errb.Reset()
	if code := doctorCmd(&out, &errb, []string{"--config", cfg.Path(), "--org", "Org", "--token-file", tf}); code != 0 {
		t.Fatalf("exit %d: %s %s", code, out.String(), errb.String())
	}
	if !strings.Contains(seen, tok) {
		t.Errorf("the API never saw the token file's token: %q", seen)
	}
	if strings.Contains(out.String()+errb.String(), tok) {
		t.Error("token printed")
	}
	if got, _ := config.Load(cfg.Path()); got.GitHubTokenFile != "" {
		t.Error("doctor must not rewrite config.json")
	}
}

func TestStatus_WithoutAServicePrintsRoutingAndRepoState(t *testing.T) {
	cfgs := routedHost(t, []string{"personal", "projects"}, "personal", nil)
	old := statusFn
	statusFn = func() (string, error) { return "", service.ErrNotInstalled }
	t.Cleanup(func() { statusFn = old })
	code, out := captureStdout(t, func() int { return runStatus(nil) })
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if strings.Count(out, "service: not installed") != 1 {
		t.Errorf("want one 'service: not installed' line:\n%s", out)
	}
	for _, want := range []string{"routing:", "personal:", "projects:"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	// A repo in an error state makes the exit code non-zero.
	failed := loopFailedPath(cfgs["projects"].SyncDir)
	os.MkdirAll(filepath.Dir(failed), 0o700)
	os.WriteFile(failed, []byte("2026-10-25T00:00:00Z boom\n"), 0o600)
	code, out = captureStdout(t, func() int { return runStatus(nil) })
	if code == 0 || !strings.Contains(out, "boom") {
		t.Errorf("a failed loop must show and exit non-zero: %d\n%s", code, out)
	}
}

func TestStatus_WithoutAServiceSingleRepo(t *testing.T) {
	initEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv(routesEnv, "")
	old := statusFn
	statusFn = func() (string, error) { return "", service.ErrNotInstalled }
	t.Cleanup(func() { statusFn = old })
	code, out := captureStdout(t, func() int { return runStatus(nil) })
	if code != 0 || strings.TrimSpace(out) != "service: not installed" {
		t.Errorf("exit %d:\n%s", code, out)
	}
}

// Re-encrypting for a new recipient changes every blob but no text: the hook
// must not announce every file as changed.
func TestHook_RecipientsCommitIsNotReportedAsChanges(t *testing.T) {
	cfg, _, _ := agedClone(t)
	const dir = "-home-alice-src-app/memory/"
	addNotes(t, cfg.Path(), map[string]string{dir + "a.md": "alpha\n", dir + "b.md": "beta\n"})
	in := hookJSON("s1", "/home/alice/src/app", "UserPromptSubmit")
	args := []string{"--config", cfg.Path(), "--hook"}
	cycle := func() { // the daemon's cycle records HEAD as copied into ~/.claude
		head, _ := gitwt.New(cfg.SyncDir).Run("rev-parse", "HEAD")
		os.WriteFile(filepath.Join(cfg.SyncDir, ".state", "synced-head"), []byte(strings.TrimSpace(head)+"\n"), 0o600)
	}
	cycle()
	runHook(t, in, args...) // baseline
	if code, _, errs := runRcpt("--config", cfg.Path(), "add", newAgeKey(t)); code != 0 {
		t.Fatalf("add: %s", errs)
	}
	cycle()
	if code, out, errs := runHook(t, in, args...); code != 0 || out != "" {
		t.Fatalf("exit %d stdout %q stderr %q: a re-encryption is not a change", code, out, errs)
	}
	// A real edit after it is still reported, and only that file.
	addNotes(t, cfg.Path(), map[string]string{dir + "a.md": "edited\n"})
	cycle()
	_, out, errs := runHook(t, in, args...)
	if !strings.Contains(out, "a.md") || strings.Contains(out, "b.md") {
		t.Errorf("want only a.md: %s (stderr %q)", out, errs)
	}
}

func TestInit_DistilledOnlyForPersonalOrLegacy(t *testing.T) {
	// Legacy single-repo: the hint and the path are shown.
	cfg, _ := initFixture(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv(routesEnv, "")
	if !isPersonalRepo(cfg) {
		t.Error("legacy mode keeps the catalog")
	}
	// Routed: only the personal repo.
	cfgs := routedHost(t, []string{"personal", "projects"}, "personal", nil)
	if !isPersonalRepo(cfgs["personal"]) || isPersonalRepo(cfgs["projects"]) {
		t.Error("only the personal repo has a catalog")
	}
	// And init of a non-personal repo leaves no distilled/ behind.
	c2, _ := initFixture(t)
	c2.SyncDir = filepath.Join(filepath.Dir(c2.SyncDir), "sync2")
	_ = c2
	cfgP := cfgs["projects"]
	os.RemoveAll(cfgP.SyncDir)
	cfgP.RemoteURL = c2.RemoteURL
	cfgP.ClaudeProjectsDir = c2.ClaudeProjectsDir
	cfgP.MergeDriverPath = c2.MergeDriverPath
	if err := bootstrapOwned(cfgP, false, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfgP.SyncDir, "distilled")); err == nil {
		t.Error("init created distilled/ in a non-personal repo")
	}
}

// A mistyped --config must fail, never fall back to the default repo.
func TestExplicitMissingConfigIsAnError(t *testing.T) {
	for _, routed := range []bool{false, true} {
		if routed {
			routedHost(t, []string{"personal", "projects"}, "personal", nil)
		} else {
			initEnv(t)
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", "")
			t.Setenv(routesEnv, "")
			// A default repo that a fallback would hit.
			def := config.Defaults()
			if err := os.MkdirAll(def.SyncDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := config.Save(def); err != nil {
				t.Fatal(err)
			}
		}
		typo := filepath.Join(t.TempDir(), "typo", "config.json")
		if code, _, errs := runRcpt("--config", typo, "add", newAgeKey(t)); code == 0 || !strings.Contains(errs, "typo") {
			t.Errorf("routed=%v recipients --config typo: exit %d %q", routed, code, errs)
		}
		if code, _ := captureStdout(t, func() int { return runDistill([]string{"--config", typo, "--prune"}) }); code == 0 {
			t.Errorf("routed=%v distill --prune --config typo must fail", routed)
		}
		if routed {
			if _, err := os.Stat(filepath.Join(filepath.Dir(typo))); err == nil {
				t.Error("nothing may be created for a typo")
			}
		}
	}
}

// With no identity, a real edit must still be reported, never hidden as
// "unchanged" because both ends failed to decrypt the same way.
func TestHook_UndecryptableChangeIsStillReported(t *testing.T) {
	cfg, repo, _ := agedClone(t)
	const dir = "-home-alice-src-app/memory/"
	addNotes(t, cfg.Path(), map[string]string{dir + "a.md": "alpha\n"})
	in := hookJSON("s1", "/home/alice/src/app", "UserPromptSubmit")
	args := []string{"--config", cfg.Path(), "--hook"}
	runHook(t, in, args...) // baseline
	addNotes(t, cfg.Path(), map[string]string{dir + "a.md": "edited\n"})
	if err := os.Remove(agecrypt.IdentityPath(repo)); err != nil {
		t.Fatal(err)
	}
	code, out, errs := runHook(t, in, args...)
	if code != 0 || !strings.Contains(out, "a.md") {
		t.Errorf("exit %d stdout %q stderr %q: an unreadable change must be reported", code, out, errs)
	}
}

func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	f()
	w.Close()
	os.Stderr = old
	b, _ := io.ReadAll(r)
	return string(b)
}

func TestInit_NonPersonalRepoPrintsNoCatalogHint(t *testing.T) {
	cfgs := routedHost(t, []string{"personal", "projects"}, "personal", nil)
	for name, want := range map[string]bool{"projects": false, "personal": true} {
		c, _ := initFixture(t)
		cfg := cfgs[name]
		os.RemoveAll(cfg.SyncDir)
		cfg.RemoteURL, cfg.ClaudeProjectsDir, cfg.MergeDriverPath = c.RemoteURL, c.ClaudeProjectsDir, c.MergeDriverPath
		t.Setenv("HOME", filepath.Dir(filepath.Dir(cfg.SyncDir)))
		errs := captureStderr(t, func() {
			if err := bootstrapOwned(cfg, false, nil, nil); err != nil {
				t.Fatal(err)
			}
		})
		if got := strings.Contains(errs, "distilled") || strings.Contains(errs, "/distill skills"); got != want {
			t.Errorf("%s: catalog hint shown = %v, want %v:\n%s", name, got, want, errs)
		}
	}
}

func TestRoutedDoctor_TokenFileAppliesToEveryRepo(t *testing.T) {
	const tok = "routed-file-token"
	seen := ""
	privateRouted(t, map[string]string{
		"a": "git@github.com:Org/mem-project-a.git",
		"b": "git@github.com:Org/mem-project-b.git",
	}, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		routedAPI("")(w, r)
	})
	t.Setenv("MEMSYNC_GITHUB_TOKEN", "")
	tf := filepath.Join(t.TempDir(), "tok")
	os.WriteFile(tf, []byte(tok+"\n"), 0o600)
	var out, errb bytes.Buffer
	if code := doctorCmd(&out, &errb, []string{"--org", "Org"}); code == 0 {
		t.Fatal("without any token the routed doctor must fail")
	}
	out.Reset()
	errb.Reset()
	if code := doctorCmd(&out, &errb, []string{"--org", "Org", "--token-file", tf}); code != 0 {
		t.Fatalf("exit %d: %s %s", code, out.String(), errb.String())
	}
	if !strings.Contains(seen, tok) {
		t.Errorf("API saw %q", seen)
	}
}

func TestStatus_SingleRepoLoopFailedAndUnknownServiceError(t *testing.T) {
	initEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv(routesEnv, "")
	old := statusFn
	t.Cleanup(func() { statusFn = old })
	cfg := config.Defaults()
	statusFn = func() (string, error) { return "running", nil }
	failed := loopFailedPath(cfg.SyncDir)
	os.MkdirAll(filepath.Dir(failed), 0o700)
	os.WriteFile(failed, []byte("2026-10-25T00:00:00Z boom\n"), 0o600)
	if code, out := captureStdout(t, func() int { return runStatus(nil) }); code == 0 || !strings.Contains(out, "boom") {
		t.Errorf("single repo with a failed loop: exit %d\n%s", code, out)
	}
	os.Remove(failed)
	statusFn = func() (string, error) { return "", errors.New("dbus gone") }
	if code, out := captureStdout(t, func() int { return runStatus(nil) }); code == 0 || !strings.Contains(out, "dbus gone") {
		t.Errorf("unknown service error: exit %d\n%s", code, out)
	}
}
