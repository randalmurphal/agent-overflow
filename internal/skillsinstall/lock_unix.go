//go:build !windows

package skillsinstall

import (
	"os"
	"syscall"
)

// lockFile blocks until this process holds the exclusive advisory lock on
// f. flock belongs to the open file description, so two opens in one
// process exclude each other as two processes do.
func lockFile(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			return err
		}
	}
}

func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
