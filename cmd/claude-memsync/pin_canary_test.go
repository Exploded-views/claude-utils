package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"

	"github.com/MarimerLLC/claude-utils/internal/testenv"
)

// The trust check asks git about the repo config.json lies in
// (`ls-files --error-unmatch` and others), and git runs a repo-local
// core.fsmonitor for that. The call must run nothing a repo's .git/config names.
func TestConfigTrusted_RepoFsmonitorNeverRuns(t *testing.T) {
	cfgs, _ := realisticHost(t, 1)
	cfg := cfgs["r00"]
	keepConfigOut(t, cfg)
	script, ran := canaryScript(t)
	repo := gitwt.New(cfg.SyncDir)
	for k, v := range map[string]string{"core.fsmonitor": script, "core.hooksPath": filepath.Dir(script), "core.askpass": script} {
		if _, err := repo.Run("config", k, v); err != nil {
			t.Fatal(err)
		}
	}
	os.Remove(ran)
	if err := configTrusted(cfg); err != nil {
		t.Fatalf("the config is not trusted: %v", err)
	}
	if exists(ran) {
		b, _ := os.ReadFile(ran)
		t.Errorf("the trust check ran a repo-local program: %s", b)
	}
}

// The hook, on a host with a registry (where the primary repo keeps its trust
// check) and on a plain one, runs no fsmonitor from any repo's config, and the
// note is still reported.
func TestHook_RepoFsmonitorNeverRuns(t *testing.T) {
	cfgs := registryHost(t)
	script, ran := canaryScript(t)
	in := hookJSON("s-fsm", "/proj/x", "UserPromptSubmit")
	addNotes(t, cfgs["a"].Path(), map[string]string{"-proj-x/memory/base.md": "b"})
	runHook(t, in, "--hook", "--deadline", "0")
	addNotes(t, cfgs["a"].Path(), map[string]string{"-proj-x/memory/news.md": "n"})
	for _, c := range cfgs {
		if _, err := gitwt.New(c.SyncDir).Run("config", "core.fsmonitor", script); err != nil {
			t.Fatal(err)
		}
	}
	os.Remove(ran)
	runHook(t, in, "--hook", "--deadline", "0")
	if exists(ran) {
		b, _ := os.ReadFile(ran)
		t.Errorf("the hook ran a repo-local fsmonitor: %s", b)
	}
}

// pinOwnDrivers names this build's programs for the filter, textconv and merge
// drivers, so a repo config that names others loses by name.
func TestPinOwnDrivers(t *testing.T) {
	t.Cleanup(func() { gitwt.SetConfigPins() })
	gitwt.SetConfigPins()
	pinOwnDrivers()
	args := strings.Join(gitwt.Command(gitwt.Opts{}, "status").Args[1:], "\n")
	for _, k := range []string{"filter.memage.process=", "filter.memage.required=true", "diff.memage.textconv="} {
		if !strings.Contains(args, "\n"+k) {
			t.Errorf("%s is not pinned:\n%s", k, args)
		}
	}
}

// main pins the drivers before it dispatches any subcommand.
func TestMainPinsOwnDrivers(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(testenv.SrcDir(), "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	i, j := strings.Index(s, "pinOwnDrivers()"), strings.Index(s, "switch os.Args[1]")
	if i < 0 || j < 0 || i > j {
		t.Error("main must call pinOwnDrivers() before the subcommand switch")
	}
}
