//go:build unix

package main

import "syscall"

// openStateFlags opens a state file without following a symlink and without
// blocking on a FIFO.
const openStateFlags = syscall.O_NOFOLLOW | syscall.O_NONBLOCK
