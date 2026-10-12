//go:build !windows

package deviceclient

import (
	"errors"
	"os"
	"syscall"
)

func tryProfileLock(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return false, nil
	}
	return err == nil, err
}

// unlockProfile drops the lock tryProfileLock took. flock belongs to the open
// file description, so a forked child's copy keeps it until exec.
func unlockProfile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
