package route

import (
	"fmt"
	"os"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/testenv"
)

func TestMain(m *testing.M) {
	cleanup := testenv.SmallTempDir()
	// Budget gates against a release's binaries (MEMSYNC_BUDGET_BIN_DIR and
	// MEMSYNC_BUDGET_TAG): check them and isolate HOME before any test runs.
	isolated, err := testenv.PrebuiltSetup()
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAIL prebuilt budget set-up:", err)
		cleanup()
		os.Exit(1)
	}
	code := m.Run()
	isolated()
	cleanup()
	os.Exit(code)
}
