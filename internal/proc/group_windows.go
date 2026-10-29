package proc

import (
	"os"
	"os/exec"
)

func setGroup(c *exec.Cmd) {}

func killGroup(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}
