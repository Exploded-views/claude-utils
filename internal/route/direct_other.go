//go:build !unix

package route

import "os"

// Without owner and device ids the direct read cannot rule out what git's
// ownership and filesystem-boundary checks would, so it always asks git.
func fileIDs(fi os.FileInfo) (uid, dev uint64, ok bool) { return 0, 0, false }

func currentUID() (uint64, bool) { return 0, false }
