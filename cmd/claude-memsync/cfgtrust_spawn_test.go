//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// The trust check runs on every config load, so at daemon start it runs many
// times: it costs four git calls (inside a work tree, the index listing, HEAD
// probed once, the HEAD listing), each with -C so the budget test B9 sees where
// it ran.
func TestConfigTrusted_CostsFourGitCalls(t *testing.T) {
	cfg, _ := trustFixture(t)
	if _, err := gitwt.New(cfg.SyncDir).Run("commit", "--allow-empty", "-q", "-m", "root"); err != nil {
		t.Fatal(err)
	}
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	shim := "#!/bin/sh\necho \"$@\" >> " + log + "\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(shim), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := configTrusted(cfg); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	calls := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(calls) != 4 {
		t.Errorf("configTrusted made %d git calls, want 4:\n%s", len(calls), b)
	}
	for _, c := range calls {
		if !strings.HasPrefix(c, "-C ") {
			t.Errorf("a git call without -C: %q", c)
		}
	}
}
