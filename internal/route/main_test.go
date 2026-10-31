package route

import (
	"fmt"
	"os"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/testenv"
)

func TestMain(m *testing.M) {
	cleanup := testenv.SmallTempDir()
	gitCleanup, err := testenv.IsolateGitMain()
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAIL git isolation:", err)
		os.Exit(1)
	}
	// Budget gates against a release's binaries (MEMSYNC_BUDGET_BIN_DIR and
	// MEMSYNC_BUDGET_TAG): check them and isolate HOME before any test runs.
	isolated, err := testenv.PrebuiltSetup()
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAIL prebuilt budget set-up:", err)
		gitCleanup()
		cleanup()
		os.Exit(1)
	}
	code := m.Run()
	isolated()
	gitCleanup()
	cleanup()
	os.Exit(code)
}
