//go:build windows

package main

import "io/fs"

// ownedBy: Windows has no uid; the user service is not used there.
func ownedBy(fs.FileInfo, int) bool { return false }
