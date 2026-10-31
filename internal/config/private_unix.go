//go:build !windows

package config

import (
	"fmt"
	"os"
	"syscall"
)

// currentUID is the uid a config file must belong to; a variable so a test
// can stand in for another owner.
var currentUID = os.Getuid

// checkPrivate refuses a config file that another user could read or write, or
// that belongs to another user. st is the fstat of the open file, so the
// answer is about the bytes that are then read, and holds wherever the file is
// (a symlink was followed by the open).
func checkPrivate(path string, st os.FileInfo) error {
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%w: %s has mode %04o, which lets other users read or write it; it must be 0600 (run: chmod 600 %s)", ErrNotPrivate, path, perm, path)
	}
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%w: cannot tell who owns %s", ErrNotPrivate, path)
	}
	if int(s.Uid) != currentUID() {
		return fmt.Errorf("%w: %s is owned by uid %d, not the current user (uid %d); only a config.json this user owns is read", ErrNotPrivate, path, s.Uid, currentUID())
	}
	return nil
}
