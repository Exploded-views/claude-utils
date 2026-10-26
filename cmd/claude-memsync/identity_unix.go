//go:build !windows

package main

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// identityModeOK: nothing beyond the owner's read and write.
func identityModeOK(st fs.FileInfo) error {
	if st.Mode().Perm()&^0o600 != 0 {
		return errors.New("the identity file must be mode 0600 (chmod 600 it)")
	}
	return nil
}

// identityOwnerLinksOK: the file is the current user's and has one name.
func identityOwnerLinksOK(st fs.FileInfo) error {
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok || int(s.Uid) != os.Getuid() {
		return errors.New("the identity file is not owned by the current user")
	}
	if s.Nlink != 1 {
		return errors.New("the identity file has more than one hard link")
	}
	return nil
}
