//go:build unix

package browser

import (
	"errors"
	"os"
	"syscall"
)

// lockRun takes the exclusive lock on a run directory's lock file without
// waiting, and reports whether it took it: false while another open of
// the file, in this process or another, holds it.
func lockRun(lock *os.File) (bool, error) {
	err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}

// unlockRun lets go of a lock lockRun took.
func unlockRun(lock *os.File) error {
	return syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
}
