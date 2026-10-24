package main

// Counts git processes per hook run, so the per-prompt cost stays small.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReview_HookGitProcessCount(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	cfg, _ := loadDistillConfig(cfgPath)
	ignoreState(t, cfg.SyncDir)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	shimDir := t.TempDir()
	count := filepath.Join(shimDir, "count")
	shim := "#!/bin/sh\necho \"$*\" >> " + count + "\nexec " + realGit + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0700); err != nil {
		t.Fatal(err)
	}
	in := hookJSON("sess-count", "/home/alice/src/app", "UserPromptSubmit")
	run := func(label string) {
		os.Remove(count)
		old := os.Getenv("PATH")
		os.Setenv("PATH", shimDir+string(os.PathListSeparator)+old)
		start := time.Now()
		code, out, errs := runHook(t, in, "--config", cfgPath, "--hook")
		d := time.Since(start)
		os.Setenv("PATH", old)
		b, _ := os.ReadFile(count)
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		t.Logf("%s: exit %d, %d git processes, %v, out=%d bytes, stderr=%q\n  %s", label, code, len(lines), d, len(out), errs, strings.Join(lines, "\n  "))
	}
	run("first run")
	run("steady, nothing new")
	addNotes(t, cfgPath, map[string]string{"-home-alice-src-app/memory/new.md": "n"})
	mem := filepath.Join(cfg.ClaudeProjectsDir, "-home-alice-src-app", "memory")
	os.MkdirAll(mem, 0700)
	os.WriteFile(filepath.Join(mem, "new.md"), []byte("n"), 0600)
	run("one change")
}
