//go:build unix

package main

import (
	"errors"
	"os"
	"syscall"
)

func openHarnessLock(path string, mode os.FileMode) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW, uint32(mode.Perm()))
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, errors.New("create lock file descriptor")
	}
	return file, nil
}

// lockFileExclusiveNonBlocking takes a BSD advisory lock (flock) on the
// open descriptor. Reports (false, nil) when another process holds it —
// which is the ordinary refusal, not an error.
//
// flock rather than fcntl/POSIX record locking on purpose: a POSIX lock
// is released by ANY close of ANY descriptor on the file in this process,
// so an unrelated read of the lock file would silently drop it. flock is
// tied to the open file description, so only the descriptor we keep can
// release it — and the kernel releases it on process death however that
// death happens, which is what makes a crashed boot recoverable with no
// stale-pid reaping.
func lockFileExclusiveNonBlocking(file *os.File) (bool, error) {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return false, nil
	}
	return false, err
}
