//go:build !windows

package ghapp

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// ownerAndLinksOK: the file is the current user's and has no other hard link
// (a second name could be a tracked file's). what names the file in errors.
func ownerAndLinksOK(st fs.FileInfo, what string) error {
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok || int(s.Uid) != os.Getuid() {
		return errors.New(what + " is not owned by the current user")
	}
	if s.Nlink != 1 {
		return errors.New(what + " has more than one hard link")
	}
	return nil
}
