package related

import (
	"os"
	"testing"
)

// TestMain turns off git's automatic gc and maintenance for every git this
// package's tests start. A fetch otherwise spawns `git maintenance run --auto
// --detach`, which outlives the test and writes into t.TempDir during cleanup.
func TestMain(m *testing.M) {
	for k, v := range map[string]string{
		"GIT_CONFIG_COUNT":   "2",
		"GIT_CONFIG_KEY_0":   "maintenance.auto",
		"GIT_CONFIG_VALUE_0": "false",
		"GIT_CONFIG_KEY_1":   "gc.auto",
		"GIT_CONFIG_VALUE_1": "0",
	} {
		os.Setenv(k, v)
	}
	os.Exit(m.Run())
}
