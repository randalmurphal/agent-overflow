package terminal

import (
	"os"

	"golang.org/x/sys/unix"
)

// pollableMaster returns the pty master as a file the runtime poller
// drives, so the output pump's read can be woken when the shell exits while
// a background job keeps the pty open (Process.awaitExit). creack/pty hands
// back a master in blocking mode: its ioctls go through File.Fd. A
// non-blocking duplicate is a fresh file the poller takes, and f is closed.
// Nothing may call Fd on the result, which would put it back in blocking
// mode; osFilePty.resize uses SyscallConn for that reason.
func pollableMaster(f *os.File) (*os.File, error) {
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
