package proc

import (
	"fmt"
	"os"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/testenv"
)

// TestMain runs every test with git confined to a temp dir: see
// testenv.IsolateGit.
func TestMain(m *testing.M) {
	cleanup, err := testenv.IsolateGitMain()
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAIL git isolation:", err)
		os.Exit(1)
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}
