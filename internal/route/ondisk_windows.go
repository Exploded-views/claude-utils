//go:build windows

package route

import "golang.org/x/sys/windows"

// onDiskName is the name path has in its directory as the file system stores
// it (FindFirstFile on the exact path returns the stored spelling). Any
// failure says ok = false and the caller keeps the spelling it has.
func onDiskName(path string) (string, bool) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", false
	}
	var data windows.Win32finddata
	h, err := windows.FindFirstFile(p, &data)
	if err != nil {
		return "", false
	}
	_ = windows.FindClose(h)
	name := windows.UTF16ToString(data.FileName[:])
	if name == "" {
		return "", false
	}
	return name, true
}
