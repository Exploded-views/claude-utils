package agecrypt

import (
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// PinDrivers gives every later git command the same filter, textconv and merge
// settings Configure writes, so the running build's programs win by name.
func TestPinDrivers_SameSettingsAsConfigure(t *testing.T) {
	t.Cleanup(func() { gitwt.SetConfigPins() })
	PinDrivers(Opts{MemsyncExe: "/opt/ms", MemmergeExe: "/opt/mm"})
	args := strings.Join(gitwt.Command(gitwt.Opts{}, "status").Args[1:], "\n")
	for _, p := range driverPairs(Opts{MemsyncExe: "/opt/ms", MemmergeExe: "/opt/mm"}) {
		if !strings.Contains(args, "\n-c\n"+p[0]+"="+p[1]+"\n") {
			t.Errorf("%s is not pinned:\n%s", p[0], args)
		}
	}
	for _, k := range []string{"filter.memage.process", "filter.memage.required", "diff.memage.textconv",
		"merge.claude-memory-index.driver", "merge.memtext.driver", "merge.memunion.driver"} {
		if !strings.Contains(args, "\n"+k+"=") {
			t.Errorf("%s is not pinned", k)
		}
	}
	PinDrivers(Opts{MemsyncExe: "/opt/ms"})
	if args := strings.Join(gitwt.Command(gitwt.Opts{}, "status").Args[1:], "\n"); strings.Contains(args, "merge.") {
		t.Errorf("merge drivers pinned without a claude-memmerge:\n%s", args)
	}
}
