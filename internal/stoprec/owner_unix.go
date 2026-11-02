//go:build !windows

package stoprec

import (
	"os"
	"syscall"
)

// oNoFollow makes a create fail rather than follow a symbolic link.
const oNoFollow = syscall.O_NOFOLLOW

func ownedByMe(st os.FileInfo) bool {
	s, ok := st.Sys().(*syscall.Stat_t)
	return ok && int(s.Uid) == currentUID()
}

// isWindowsChmod is false off Windows: a failed Chmod there is a real error.
func isWindowsChmod(error) bool { return false }
