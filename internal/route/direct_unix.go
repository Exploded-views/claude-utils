//go:build unix

package route

import (
	"os"
	"syscall"
)

// fileIDs is the owner and device of a file. ok is false when the host does not
// say; the direct read then asks git.
func fileIDs(fi os.FileInfo) (uid, dev uint64, ok bool) {
	st, isStat := fi.Sys().(*syscall.Stat_t)
	if !isStat {
		return 0, 0, false
	}
	return uint64(st.Uid), uint64(st.Dev), true //nolint:unconvert
}

func currentUID() (uint64, bool) {
	if u := os.Geteuid(); u >= 0 {
		return uint64(u), true
	}
	return 0, false
}
