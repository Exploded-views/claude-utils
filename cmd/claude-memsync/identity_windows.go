//go:build windows

package main

import "io/fs"

// Windows has no mode bits, uid or link count to check here; the file's ACL
// is the user's.
func identityModeOK(fs.FileInfo) error       { return nil }
func identityOwnerLinksOK(fs.FileInfo) error { return nil }
