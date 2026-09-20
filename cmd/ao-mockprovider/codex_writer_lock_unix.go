//go:build unix

package main

import (
	"errors"
	"os"
	"syscall"
)

// tryLockWriterFile takes the exclusive OS lock upstream holds on a loaded
// thread's lock file. held=false means another live process has it.
func tryLockWriterFile(file *os.File) (held bool, err error) {
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return false, err
}
