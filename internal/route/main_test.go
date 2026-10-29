package route

import (
	"os"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/testenv"
)

func TestMain(m *testing.M) {
	cleanup := testenv.SmallTempDir()
	code := m.Run()
	cleanup()
	os.Exit(code)
}
