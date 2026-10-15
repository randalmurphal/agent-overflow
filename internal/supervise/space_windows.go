//go:build windows

package supervise

import "golang.org/x/sys/windows"

// ReadDiskSpace reads the volume that holds path.
func ReadDiskSpace(path string) (DiskSpace, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return DiskSpace{}, err
	}
	var available, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(name, &available, &total, &free); err != nil {
		return DiskSpace{}, err
	}
	return DiskSpace{Available: available, Used: total - free}, nil
}
