//go:build linux

package supervise

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// platformCloneFile clones in into destination with the FICLONE ioctl, which
// btrfs, XFS with reflink and bcachefs answer by sharing extents. Every other
// filesystem refuses, and the caller copies instead.
func platformCloneFile(in *os.File, destination string) (bool, error) {
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, filePerm)
	if err != nil {
		return false, err
	}
	if err := unix.IoctlFileClone(int(out.Fd()), int(in.Fd())); err != nil {
		closeErr := out.Close()
		if cloneUnsupported(err) {
			return false, closeErr
		}
		return false, errors.Join(err, closeErr)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return false, err
	}
	return true, out.Close()
}

// cloneUnsupported reports a refusal that means "this filesystem, or this
// pair of files, cannot share extents" rather than an I/O failure.
func cloneUnsupported(err error) bool {
	for _, errno := range []error{unix.EOPNOTSUPP, unix.ENOTSUP, unix.EXDEV, unix.EINVAL, unix.ENOTTY, unix.ENOSYS} {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}
