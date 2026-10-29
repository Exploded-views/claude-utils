//go:build !windows

package proc

import (
	"os/exec"
	"syscall"
)

func setGroup(c *exec.Cmd) {
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	c.SysProcAttr.Setpgid = true
}

// killGroup kills the group the child leads (a child started after Group); a
// child that is not a group leader is killed alone.
func killGroup(pid int) {
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}
