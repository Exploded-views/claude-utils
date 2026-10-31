package proc

import (
	"errors"
	"os/exec"
	"testing"
)

// A release that comes late, after another hook began in the same process,
// must not undo the new hook's group or re-allow its children.
func TestReleaseGenIgnoresAStaleGeneration(t *testing.T) {
	old := GroupGen()
	cur := GroupGen() // another hook began
	KillAll()
	ReleaseGen(old)
	if err := Run(exec.Command("true")); !errors.Is(err, ErrStopped) {
		t.Errorf("a stale ReleaseGen re-allowed children: %v", err)
	}
	if !grouped.Load() {
		t.Error("a stale ReleaseGen undid the new hook's group")
	}
	ReleaseGen(cur)
	if err := Run(exec.Command("true")); err != nil {
		t.Errorf("the current ReleaseGen did not release: %v", err)
	}
}
