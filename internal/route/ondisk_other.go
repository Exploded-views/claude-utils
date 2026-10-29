//go:build !darwin && !windows

package route

// onDiskName: the other hosts match names case-sensitively, so the spelling
// asked for is the one on disk.
func onDiskName(path string) (string, bool) { return "", false }
