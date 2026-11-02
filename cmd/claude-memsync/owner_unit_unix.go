//go:build !windows

package main

import (
	"io/fs"
	"syscall"
)

// platformOwner is the owner uid and group gid of a file.
func platformOwner(st fs.FileInfo) (uid, gid int, ok bool) {
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(s.Uid), int(s.Gid), true
}
