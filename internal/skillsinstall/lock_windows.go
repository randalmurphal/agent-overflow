//go:build windows

package skillsinstall

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockFile blocks until this process holds the lock on the first byte of f,
// shared or exclusive.
func lockFile(f *os.File, shared bool) error {
	var flags uint32 = windows.LOCKFILE_EXCLUSIVE_LOCK
	if shared {
		flags = 0
	}
	return windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, &windows.Overlapped{})
}

func unlockFile(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{})
}
