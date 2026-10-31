package testenv

import (
	"fmt"
	"os"
	"testing"
)

// TestMain runs every test with git confined to a temp dir: see
// IsolateGit.
func TestMain(m *testing.M) {
	cleanup, err := IsolateGitMain()
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAIL git isolation:", err)
		os.Exit(1)
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}
