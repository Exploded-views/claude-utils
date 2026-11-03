//go:build windows

package main

import "io/fs"

// platformOwner: Windows has no uid; the user service is not used there.
func platformOwner(fs.FileInfo) (int, int, bool) { return 0, 0, false }
