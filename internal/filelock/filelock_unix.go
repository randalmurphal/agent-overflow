//go:build !windows

package filelock

import (
	"errors"
	"os"
	"syscall"
)

// TryLock takes the exclusive lock on f without waiting. It reports false
// when another open file holds it.
func TryLock(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return false, nil
	}
	return err == nil, err
}

// Unlock drops the lock TryLock took.
func Unlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
