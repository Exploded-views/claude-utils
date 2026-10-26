//go:build linux

package main

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestHardenProcess_ClearsDumpableOnLinux(t *testing.T) {
	before, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { unix.Prctl(unix.PR_SET_DUMPABLE, uintptr(before), 0, 0, 0) })
	hardenProcess()
	got, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
	if err != nil || got != 0 {
		t.Fatalf("PR_GET_DUMPABLE = %d, %v; want 0", got, err)
	}
}
