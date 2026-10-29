//go:build budget

package route

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// logLoad logs the 1-minute load average at the start and end of a budget test.
func logLoad(t *testing.T) {
	t.Helper()
	t.Logf("load average at start: %.2f", loadAvg1())
	t.Cleanup(func() { t.Logf("load average at end: %.2f", loadAvg1()) })
}

func loadAvg1() float64 {
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			if v, err := strconv.ParseFloat(f[0], 64); err == nil {
				return v
			}
		}
	}
	out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return -1
	}
	if f := strings.Fields(strings.Trim(string(out), "{} \n")); len(f) > 0 {
		if v, err := strconv.ParseFloat(f[0], 64); err == nil {
			return v
		}
	}
	return -1
}
