//go:build windows

package main

import "os"

// Windows has no unix permission bits; report the expected mode.
func runtimeUnixPerm(os.FileInfo) os.FileMode { return 0o600 }
