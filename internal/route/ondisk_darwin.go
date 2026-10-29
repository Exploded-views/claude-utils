//go:build darwin

package route

import (
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/unix"
)

// onDiskName is the name path has in its directory, spelled as the file system
// stores it: the volume matches names without regard to case, so the spelling
// asked for can differ. F_GETPATH on an open descriptor gives the real path;
// its last component is the name. A symlink (not followed) or any failure says
// ok = false and the caller keeps the spelling it has.
func onDiskName(path string) (string, bool) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", false
	}
	defer unix.Close(fd)
	var buf [unix.PathMax]byte
	if _, _, e := unix.Syscall(unix.SYS_FCNTL, uintptr(fd), unix.F_GETPATH, uintptr(unsafe.Pointer(&buf[0]))); e != 0 {
		return "", false
	}
	n := 0
	for n < len(buf) && buf[n] != 0 {
		n++
	}
	if n == 0 || n == len(buf) {
		return "", false
	}
	return filepath.Base(string(buf[:n])), true
}
