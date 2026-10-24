package agecrypt

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Paths with shell metacharacters must survive git's sh invocation.
func TestShellQuote_RunsThroughSh(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	for _, name := range []string{"plain", "with space", "it's", "d$ollar", "am&per", "per%cent", "all ' $ & % x"} {
		dir := filepath.Join(t.TempDir(), name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		exe := filepath.Join(dir, "prog")
		os.WriteFile(exe, []byte("#!/bin/sh\nprintf '%s|' \"$@\"\n"), 0o700)
		out, err := exec.Command("sh", "-c", ShellQuote(exe)+" one two").Output()
		if err != nil || string(out) != "one|two|" {
			t.Errorf("%q: %q %v", name, out, err)
		}
	}
}

func TestMergeCommand_EscapesPercent(t *testing.T) {
	got := MergeCommand("/odd dir/50%/it's/memmerge", "--text")
	if !strings.Contains(got, "50%%") || strings.Count(got, "%O %A %B %L %P") != 1 {
		t.Errorf("percent in the path must be doubled, placeholders kept: %s", got)
	}
}
