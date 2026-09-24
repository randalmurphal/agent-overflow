//go:build windows

package supervise

import "golang.org/x/sys/windows"

// FreeBytes is the space this user can still write on the volume that holds
// path.
func FreeBytes(path string) (uint64, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var available, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(name, &available, &total, &free); err != nil {
		return 0, err
	}
	return available, nil
}
