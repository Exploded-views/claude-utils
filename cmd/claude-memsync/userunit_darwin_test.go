//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// The generated launchd plist must start the agent at login (RunAtLoad) and
// restart it (KeepAlive). kardianos finds the home directory from the user
// database, not $HOME, so this writes ~/Library/LaunchAgents of the account
// running the tests: it only runs in CI (a throwaway runner) and refuses to
// touch an existing plist.
func TestLaunchAgentPlist_RunAtLoadAndKeepAlive(t *testing.T) {
	if os.Getenv("CI") == "" {
		t.Skip("writes the real ~/Library/LaunchAgents; set CI=1 on a throwaway machine")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "launchctl"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	svc, err := userSvc("/x/config.json")
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(home, "Library", "LaunchAgents", "claude-memsync.plist")
	if _, err := os.Stat(plist); err == nil {
		t.Skip("a plist already exists; not overwriting it")
	}
	t.Cleanup(func() { os.Remove(plist) })
	if err := kardianosInstall(svc); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(plist)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"RunAtLoad", "KeepAlive"} {
		if !regexp.MustCompile(`<key>` + key + `</key>\s*<true/>`).Match(b) {
			t.Errorf("plist lacks %s=true:\n%s", key, b)
		}
	}
}
