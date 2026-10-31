package related

import (
	"fmt"
	"os"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/testenv"
)

// TestMain turns off git's automatic gc and maintenance for every git this
// package's tests start. A fetch otherwise spawns `git maintenance run --auto
// --detach`, which outlives the test and writes into t.TempDir during cleanup.
func TestMain(m *testing.M) {
	gitCleanup, err := testenv.IsolateGitMain()
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAIL git isolation:", err)
		os.Exit(1)
	}
	for k, v := range map[string]string{
		"GIT_CONFIG_COUNT":   "2",
		"GIT_CONFIG_KEY_0":   "maintenance.auto",
		"GIT_CONFIG_VALUE_0": "false",
		"GIT_CONFIG_KEY_1":   "gc.auto",
		"GIT_CONFIG_VALUE_1": "0",
	} {
		os.Setenv(k, v)
	}
	code := m.Run()
	gitCleanup()
	os.Exit(code)
}
