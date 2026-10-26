//go:build linux

package main

import "golang.org/x/sys/unix"

func hardenDumpable() { _ = unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0) }
