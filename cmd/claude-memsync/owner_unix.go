//go:build !windows

package main

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// ownerAndLinksOK: the file is the current user's and has no other hard link
// (a second name could be a tracked file's).
func ownerAndLinksOK(st fs.FileInfo) error {
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok || int(s.Uid) != os.Getuid() {
		return errors.New("config.json is not owned by the current user")
	}
	if s.Nlink != 1 {
		return errors.New("config.json has more than one hard link")
	}
	return nil
}
