package main

import (
	"bytes"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/proc"
)

// stdoutProbe notes, at the first write, whether proc.KillAll already ran
// (proc.Run refuses new children after it).
type stdoutProbe struct {
	bytes.Buffer
	probed, killed bool
}

func (p *stdoutProbe) Write(b []byte) (int, error) {
	if !p.probed {
		p.probed = true
		p.killed = errors.Is(proc.Run(exec.Command("true")), proc.ErrStopped)
	}
	return p.Buffer.Write(b)
}

// At the deadline, no git outlives it: the children are killed before the
// held-repo alert and its marker are written, not after that file I/O.
func TestHook_ExpiryKillsGitBeforeTheHeldAlertIsWritten(t *testing.T) {
	cfgPath, _ := changesRepo(t)
	writeRecord(t, cfgPath, "2026-10-26T02:50:29Z "+heldMarker+"check failed: HTTP 404\n")
	child := make(chan *exec.Cmd, 1)
	hookTestHook = func() { // a hanging git, started by the worker
		c := proc.Command("sleep", "60")
		child <- c
		_ = proc.Run(c)
	}
	t.Cleanup(func() { hookTestHook = nil })

	var out stdoutProbe
	var errb bytes.Buffer
	in := hookJSON("sess-o", "/home/alice/src/app", "UserPromptSubmit")
	code := changesHook(strings.NewReader(in), &out, &errb, cfgPath, "", "", 0, 300*time.Millisecond)
	select {
	case c := <-child:
		t.Cleanup(func() { _ = c.Process.Kill() })
	default:
		t.Fatal("the fake git never started")
	}
	if code != 0 || !strings.Contains(out.String(), "is HELD") {
		t.Fatalf("exit %d, stdout %q; want the held alert on the deadline path", code, out.String())
	}
	if !out.killed {
		t.Error("the held alert was written before proc.KillAll ran: a git child outlives the deadline by the alert's I/O")
	}
}
