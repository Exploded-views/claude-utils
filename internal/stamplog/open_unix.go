//go:build !windows

package stamplog

import "syscall"

const nonblock = syscall.O_NONBLOCK
