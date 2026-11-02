//go:build !windows

package main

import (
	"io/fs"
	"syscall"
)

// ownedBy reports a file owned by uid.
func ownedBy(st fs.FileInfo, uid int) bool {
	s, ok := st.Sys().(*syscall.Stat_t)
	return ok && int(s.Uid) == uid
}
