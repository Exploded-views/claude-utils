//go:build !windows

package main

import "os"

func runtimeUnixPerm(fi os.FileInfo) os.FileMode { return fi.Mode().Perm() }
