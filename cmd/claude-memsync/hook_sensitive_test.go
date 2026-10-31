package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/proc"
)

// keepConfigOut keeps config.json out of the repo, as init does: routedHost's
// later commits (AddAll) would otherwise track it and make the trust check
// drop its keys.
func keepConfigOut(t *testing.T, cfg config.Config) {
	t.Helper()
	ex := filepath.Join(cfg.SyncDir, ".git", "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(ex), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ex, []byte("/config.json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// canaryScript is an executable that records, in the returned file, each time
// it runs.
func canaryScript(t *testing.T) (script, ran string) {
	t.Helper()
	dir := t.TempDir()
	ran = filepath.Join(dir, "ran")
	script = filepath.Join(dir, "canary.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"$0 $*\" >> "+ran+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return script, ran
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// The hook reads and acts on none of the five keys the trust check guards
// (githubTokenCmd, githubTokenFile, githubApp, githubApiUrl, mergeDriverPath):
// the configs it works with do not even hold them, so it needs no judgement of
// the file they came from. For each key, set to a canary that records any use,
// the hook's report and markers are what they are without it, no program is
// run, no request is made, and the only processes started are git.
func TestHook_SensitiveKeysAreNeverUsed(t *testing.T) {
	var hits atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "no", 500)
	}))
	t.Cleanup(api.Close)
	script, ran := canaryScript(t)
	keys := map[string]func(c *config.Config){
		"githubTokenCmd":  func(c *config.Config) { c.GitHubTokenCmd = []string{script, "token"} },
		"githubTokenFile": func(c *config.Config) { c.GitHubTokenFile = filepath.Join(filepath.Dir(script), "token") },
		"githubApp":       func(c *config.Config) { c.GitHubApp = "canary-app" },
		"githubApiUrl":    func(c *config.Config) { c.GitHubAPIURL = api.URL },
		"mergeDriverPath": func(c *config.Config) { c.MergeDriverPath = script },
	}
	for key, set := range keys {
		t.Run(key, func(t *testing.T) {
			os.Remove(ran)
			hits.Store(0)
			cfgs := routedHost(t, []string{"r00", "r01"}, "", []map[string]string{
				{"folder": "f-r00", "repo": "r00"}, {"folder": "f-r01", "repo": "r01"}})
			for name, cfg := range cfgs {
				set(&cfg)
				if err := config.Save(cfg); err != nil {
					t.Fatal(err)
				}
				keepConfigOut(t, cfg)
				cfgs[name] = cfg
			}
			requireTrusted(t, cfgs) // the key is honoured by loadConfig: the hook must not even hold it
			in := hookJSON("s-key", "", "UserPromptSubmit")
			runHook(t, in, "--hook", "--deadline", "0")
			file := noteFor(t, cfgs, "r00", "news")

			var mu sync.Mutex
			var progs []string
			restore := proc.SetSpawnHook(func(name string, _ []string) { mu.Lock(); progs = append(progs, name); mu.Unlock() })
			code, out, errs := runHook(t, in, "--hook", "--deadline", "0")
			restore()

			if code != 0 || errs != "" {
				t.Errorf("exit %d stderr %q", code, errs)
			}
			if got := strings.Count(hookContext(t, out), file); got != 1 {
				t.Errorf("the note was reported %d times, want 1:\n%s", got, out)
			}
			if markerOf(cfgs["r00"], "s-key") != fileHead(cfgs["r00"]) {
				t.Error("the marker did not move to HEAD")
			}
			if exists(ran) {
				b, _ := os.ReadFile(ran)
				t.Errorf("the canary program ran: %s", b)
			}
			if hits.Load() != 0 {
				t.Errorf("%d requests reached the canary API", hits.Load())
			}
			for _, p := range progs {
				if filepath.Base(p) != "git" {
					t.Errorf("the hook started %q, want only git", p)
					break
				}
			}
			targets, err := hookTargets("", "")
			if err != nil || len(targets) != 2 {
				t.Fatalf("hookTargets: %v (%d targets)", err, len(targets))
			}
			for _, tg := range targets {
				c := tg.cfg
				if len(c.GitHubTokenCmd) > 0 || c.GitHubTokenFile != "" || c.GitHubApp != "" || c.GitHubAPIURL != "" || c.MergeDriverPath != "" {
					t.Errorf("repo %s: the hook holds a sensitive key: %+v", tg.name, c)
				}
			}
		})
	}
}

// stripSensitive clears exactly the keys sanitizeCfg guards: both come from
// one table, so a sixth key cannot be added to one and missed by the other.
func TestStripSensitive_ClearsEveryGuardedKey(t *testing.T) {
	full := config.Defaults()
	full.ConfigPath = filepath.Join(t.TempDir(), "config.json")
	full.GitHubTokenCmd = []string{"x"}
	full.GitHubTokenFile = "f"
	full.GitHubApp = "a"
	full.GitHubAPIURL = "u"
	full.MergeDriverPath = "m"
	if got := configKeysFromFile(full); len(got) != 5 {
		t.Fatalf("configKeysFromFile sees %v, want all five keys", got)
	}
	got := stripSensitive(full)
	if keys := configKeysFromFile(got); len(keys) != 0 {
		t.Errorf("stripSensitive left %v", keys)
	}
	if got.SyncDir != full.SyncDir || got.ConfigPath != full.ConfigPath {
		t.Error("stripSensitive changed more than the guarded keys")
	}
}

// setGitCanaries makes the repo's config start the canary script as a filter,
// merge driver, textconv, external diff, fsmonitor, pager, editor, askpass, ssh
// command, hook, credential helper, gpg program, included config's fsmonitor,
// upload-pack and alternate-refs command.
func setGitCanaries(t *testing.T, repo *gitwt.Repo, script, ran string) {
	t.Helper()
	hooksDir := t.TempDir()
	for _, h := range []string{"post-checkout", "post-merge", "post-commit", "pre-commit", "reference-transaction", "post-index-change", "fsmonitor-watchman"} {
		if err := os.WriteFile(filepath.Join(hooksDir, h), []byte("#!/bin/sh\necho "+h+" >> "+ran+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	inc := filepath.Join(t.TempDir(), "included.cfg")
	if err := os.WriteFile(inc, []byte("[core]\n\tfsmonitor = "+script+"\n[filter \"i\"]\n\tclean = "+script+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	alt := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo.Dir, ".git", "objects", "info", "alternates"), []byte(alt+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"filter.c.clean": script, "filter.c.smudge": script, "filter.c.process": script, "filter.c.required": "false",
		"merge.c.driver": script + " %O %A %B", "merge.c.name": "c",
		"diff.c.textconv": script, "diff.c.command": script, "diff.external": script,
		"core.fsmonitor": script, "core.pager": script, "core.editor": script, "core.askpass": script,
		"core.sshCommand": script, "core.hooksPath": hooksDir, "credential.helper": "!" + script, "gpg.program": script,
		"include.path": inc, "remote.origin.url": "/nonexistent/origin.git", "remote.origin.uploadpack": script,
		"remote.origin.receivepack": script, "core.alternateRefsCommand": script, "uploadpack.packObjectsHook": script,
	} {
		if _, err := repo.Run("config", k, v); err != nil {
			t.Fatal(err)
		}
	}
}

// No git process the hook runs can run a filter, a merge driver, a textconv or
// external diff, an fsmonitor, a pager, a hook, a credential helper, an
// included config's program, an upload-pack or an alternate-refs command from
// the repo's config, even with .gitattributes in the tree binding the filter,
// driver and diff to every file. The canary is set as all of them after the
// commits, so only the hook's own git calls can run it. (The trust check's
// `ls-files --error-unmatch` does run core.fsmonitor, so the hook must not
// call it.)
func TestHook_GitCannotRunFilterOrDriver(t *testing.T) {
	cfgs, _ := realisticHost(t, 2)
	script, ran := canaryScript(t)
	in := hookJSON("s-git", "", "UserPromptSubmit")
	repo := gitwt.New(cfgs["r00"].SyncDir)
	if err := os.WriteFile(filepath.Join(repo.Dir, ".gitattributes"), []byte("* filter=c diff=c merge=c\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	noteFor(t, cfgs, "r00", "base") // commits .gitattributes with it
	runHook(t, in, "--hook", "--deadline", "0")
	file := noteFor(t, cfgs, "r00", "news")
	setGitCanaries(t, repo, script, ran)
	os.Remove(ran)
	_, out, errs := runHook(t, in, "--hook", "--deadline", "0")
	if got := strings.Count(hookContext(t, out), file); got != 1 {
		t.Errorf("the note was reported %d times, want 1 (stderr %q):\n%s", got, errs, out)
	}
	if exists(ran) {
		b, _ := os.ReadFile(ran)
		t.Errorf("a configured filter, driver or hook ran under the hook's git calls: %s", b)
	}
}

// registryHost is a host with a related-repos registry and a primary repo "a"
// whose config carries what init writes (mergeDriverPath, a token command), so
// the hook takes the related-repos path.
func registryHost(t *testing.T) map[string]config.Config {
	t.Helper()
	h := newRelHost(t, map[string][2]string{"a": {"mem-project-a", "project"}}, nil)
	cfg := h.cfgs["a"]
	cfg.MergeDriverPath = "/opt/bin/claude-memmerge"
	cfg.GitHubTokenCmd = []string{"/bin/echo", "t"}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	for _, c := range h.cfgs {
		keepConfigOut(t, c)
	}
	h.cfgs["a"] = cfg
	return h.cfgs
}

// On a host with a registry the hook starts no git when nothing is new: the
// related-repos check reads the primary repo's config as it is, without the
// trust check (4 git processes per prompt, one of them running core.fsmonitor).
func TestHook_RegistryHostSteadyStateSpawnsNoGit(t *testing.T) {
	cfgs := registryHost(t)
	addNotes(t, cfgs["a"].Path(), map[string]string{"-proj-x/memory/base.md": "b"})
	s := gitShim(t, "")
	in := hookJSON("s-reg", "/proj/x", "UserPromptSubmit")
	seam, shimmed, _, errs := countedHook(t, s, in, "--deadline", "0")
	requireSpawns(t, "a new session's first look", seam, shimmed, 0)
	seam, shimmed, out, errs2 := countedHook(t, s, in, "--deadline", "0")
	requireSpawns(t, "a repeat prompt with nothing new", seam, shimmed, 0)
	if out != "" || errs != "" || errs2 != "" {
		t.Errorf("printed %q / %q / %q, want nothing", out, errs, errs2)
	}
}

// The registry host (the related-repos path) runs no configured program either.
func TestHook_GitCannotRunFilterOrDriver_RegistryHost(t *testing.T) {
	cfgs := registryHost(t)
	script, ran := canaryScript(t)
	repo := gitwt.New(cfgs["a"].SyncDir)
	if err := os.WriteFile(filepath.Join(repo.Dir, ".gitattributes"), []byte("* filter=c diff=c merge=c\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	addNotes(t, cfgs["a"].Path(), map[string]string{"-proj-x/memory/base.md": "b"})
	in := hookJSON("s-reg2", "/proj/x", "UserPromptSubmit")
	runHook(t, in, "--hook", "--deadline", "0")
	addNotes(t, cfgs["a"].Path(), map[string]string{"-proj-x/memory/news.md": "n"})
	setGitCanaries(t, repo, script, ran)
	for _, c := range cfgs { // every repo of the host, the registry too
		if c.SyncDir != repo.Dir {
			setGitCanaries(t, gitwt.New(c.SyncDir), script, ran)
		}
	}
	os.Remove(ran)
	_, out, errs := runHook(t, in, "--hook", "--deadline", "0")
	if got := strings.Count(hookContext(t, out), "news.md"); got != 1 {
		t.Errorf("the note was reported %d times, want 1 (stderr %q):\n%s", got, errs, out)
	}
	if exists(ran) {
		b, _ := os.ReadFile(ran)
		t.Errorf("a configured program ran under the hook's git calls on a registry host: %s", b)
	}
}

// The one place the hook looks at the keys is the related-repos check, which
// asks whether the primary repo names a token source and where its API is: it
// keeps the full, trust-checked config of that one repo, whatever the targets
// hold.
func TestHook_RelatedCheckKeepsThePrimaryConfig(t *testing.T) {
	api := newFakeAPI(t)
	visHost(t, api, nil)
	rc, err := loadRelCtxHook("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rc.vis.cfg.GitHubAPIURL != api.srv.URL {
		t.Errorf("the related check lost the primary's githubApiUrl: %q", rc.vis.cfg.GitHubAPIURL)
	}
	if rc.vis.build != nil || !rc.vis.offline {
		t.Error("the hook's visibility check can build an API client: build must be nil and offline set")
	}
	if c, err := rc.vis.getClient(); c != nil || err != nil {
		t.Errorf("getClient gave %v, %v; want no client", c, err)
	}
	targets, err := hookTargets("", "")
	if err != nil || len(targets) == 0 {
		t.Fatalf("hookTargets: %v", err)
	}
	for _, tg := range targets {
		if tg.cfg.GitHubAPIURL != "" {
			t.Errorf("repo %s: the report path holds githubApiUrl %q", tg.name, tg.cfg.GitHubAPIURL)
		}
	}

}
