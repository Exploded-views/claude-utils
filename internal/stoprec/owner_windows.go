//go:build windows

package stoprec

import "os"

const oNoFollow = 0

// Windows has no uid in a file's info; the config dir's ACL is the user's.
func ownedByMe(os.FileInfo) bool { return true }

// isWindowsChmod: Chmod on Windows only sets the read-only bit and may fail on
// odd file systems; the record is still in the user's profile.
func isWindowsChmod(error) bool { return true }
