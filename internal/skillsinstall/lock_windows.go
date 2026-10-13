//go:build windows

package skillsinstall

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockFile blocks until this process holds the exclusive lock on the first
// byte of f.
func lockFile(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &windows.Overlapped{})
}

func unlockFile(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{})
}
