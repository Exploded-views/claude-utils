//go:build !windows

package main

import (
	"io/fs"
	"os"
	"syscall"
)

func ownedByCurrentUser(st fs.FileInfo) bool {
	s, ok := st.Sys().(*syscall.Stat_t)
	return ok && int(s.Uid) == os.Getuid()
}
