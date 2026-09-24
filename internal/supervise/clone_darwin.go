//go:build darwin

package supervise

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// platformCloneFile clones in into destination with clonefile(2), which APFS
// answers by sharing blocks. HFS+ and network volumes refuse, and the caller
// copies instead. clonefile creates its destination, so any previous one is
// removed first.
func platformCloneFile(in *os.File, destination string) (bool, error) {
	if err := os.Remove(destination); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := unix.Clonefile(in.Name(), destination, unix.CLONE_NOFOLLOW); err != nil {
		if cloneUnsupported(err) {
			return false, nil
		}
		return false, err
	}
	out, err := os.OpenFile(destination, os.O_WRONLY, 0)
	if err != nil {
		return false, err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return false, err
	}
	return true, out.Close()
}

func cloneUnsupported(err error) bool {
	for _, errno := range []error{unix.ENOTSUP, unix.EOPNOTSUPP, unix.EXDEV, unix.EINVAL, unix.ENOTTY, unix.ENOSYS} {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}
