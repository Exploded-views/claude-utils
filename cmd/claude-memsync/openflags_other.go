//go:build !unix

package main

// openStateFlags: no extra flags here; readHeldRecord's Lstat and Stat checks
// still refuse anything but a regular file.
const openStateFlags = 0
