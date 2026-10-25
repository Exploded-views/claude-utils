//go:build windows

package main

import "io/fs"

// Windows has no uid/mode bits to check; the file's ACL is the user's.
func ownerAndLinksOK(fs.FileInfo) error { return nil }
