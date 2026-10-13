//go:build !windows

package skillsinstall

import (
	"os"
	"syscall"
)

// lockFile blocks until this process holds the advisory lock on f, shared
// or exclusive. flock belongs to the open file description, so two opens in
// one process exclude each other as two processes do.
func lockFile(f *os.File, shared bool) error {
	how := syscall.LOCK_EX
	if shared {
		how = syscall.LOCK_SH
	}
	for {
		err := syscall.Flock(int(f.Fd()), how)
		if err != syscall.EINTR {
			return err
		}
	}
}

func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
