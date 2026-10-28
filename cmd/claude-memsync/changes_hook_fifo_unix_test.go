//go:build unix

package main

import "syscall"

func mkfifo(p string) error { return syscall.Mkfifo(p, 0o600) }

func syscallNonblock() int { return syscall.O_NONBLOCK }
