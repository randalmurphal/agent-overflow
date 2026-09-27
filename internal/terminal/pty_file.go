//go:build !windows

package terminal

import (
	"os"

	"golang.org/x/sys/unix"
)

// osFilePty bundles the PTY master *os.File with a typed resize method.
// Separating resize from the plain *os.File surface keeps the Setsize ioctl
// out of arbitrary call sites that only need read/write/close.
type osFilePty struct {
	*os.File
}

// resize sets the winsize through SyscallConn rather than File.Fd, which
// would put a pollable master back in blocking mode (pollableMaster).
func (o *osFilePty) resize(rows, cols uint16) error {
	raw, err := o.SyscallConn()
	if err != nil {
		return err
	}
	var ioctlErr error
	if err := raw.Control(func(fd uintptr) {
		ioctlErr = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols})
	}); err != nil {
		return err
	}
	return ioctlErr
}
