//go:build windows

package config

import "os"

// Windows has no uid or mode bits to check; the file's ACL is the user's.
func checkPrivate(string, os.FileInfo) error { return nil }
