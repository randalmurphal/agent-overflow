package procutil

import (
	"os"

	"golang.org/x/sys/unix"
)

// PollableMaster returns the pty master as a file the runtime poller drives,
// so a read of it can be woken by closing it while a process still holds the
// pty open. creack/pty hands back a master in blocking mode: its ioctls go
// through File.Fd. A non-blocking duplicate is a fresh file the poller takes,
// and f is closed. Nothing may call Fd on the result, which would put it back
// in blocking mode; use SyscallConn for an ioctl.
func PollableMaster(f *os.File) (*os.File, error) {
	fd, dupErr := unix.FcntlInt(f.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	closeErr := f.Close()
	if dupErr != nil {
		return nil, dupErr
	}
	if closeErr != nil {
		_ = unix.Close(fd)
		return nil, closeErr
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), f.Name()), nil
}
