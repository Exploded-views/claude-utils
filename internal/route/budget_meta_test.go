//go:build budget

package route

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/testenv"
)

// runGateForcedInvalid runs one gate as a subprocess of this test binary with the
// END validity check forced over the limit and the verdict forced to fail, and
// requires it to be INVALID (skipped, naming the forced check), never PASS or FAIL.
func runGateForcedInvalid(t *testing.T, gate string) {
	t.Helper()
	if testenv.Prebuilt() {
		t.Skip("the fault switches are refused in prebuilt mode, so this meta test cannot run there")
	}
	testenv.SkipIfInvalid(t) // a host that is already INVALID would skip the gate at its start, not at its end
	cmd := exec.Command(os.Args[0], "-test.run", "^"+gate+"$", "-test.v", "-test.count=1")
	cmd.Env = append(os.Environ(), testenv.EnvForceEndInvalid+"=1", testenv.EnvForceVerdictFail+"=1")
	out, err := cmd.CombinedOutput()
	o := string(out)
	if err != nil || !strings.Contains(o, "--- SKIP: "+gate) || !strings.Contains(o, "INVALID: ") ||
		!strings.Contains(o, testenv.EnvForceEndInvalid) || strings.Contains(o, "--- FAIL") || strings.Contains(o, "--- PASS: "+gate) {
		t.Errorf("%s with the end check forced INVALID: err=%v, want SKIP with an INVALID line and no FAIL/PASS:\n%s", gate, err, o)
	}
}

func TestBudgetMeta_B4ForcedEndInvalid(t *testing.T) {
	runGateForcedInvalid(t, "TestBudgetB4_NoWorkProportionalToUnrelatedDirs")
}

func TestBudgetMeta_B7ForcedEndInvalid(t *testing.T) {
	runGateForcedInvalid(t, "TestBudgetB7_BytesReadFlatAsTmpdirGrows")
}
