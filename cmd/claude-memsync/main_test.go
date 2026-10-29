package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/testenv"
)

// No test may reach a real service: commands that meet a held sync lock ask
// the service manager for its state, and a live daemon on the machine running
// the tests would otherwise be stopped. Tests that need a service install their
// own fakes (installFakeSvc).
func TestMain(m *testing.M) {
	statusFn = func() (string, error) { return "stopped", nil }
	startFn = func() error { panic("a test started a real service") }
	stopFn = func() error { panic("a test stopped a real service") }
	// Nor may one touch the real systemd user manager or unit file.
	systemctlFn = func(args ...string) error { panic("a test ran systemctl") }
	userUnitPathFn = func() (string, error) {
		return filepath.Join(os.TempDir(), "claude-memsync-test-no-such-dir", userUnitName), nil
	}
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
