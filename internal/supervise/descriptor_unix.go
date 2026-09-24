//go:build unix

package supervise

import (
	"os"

	"golang.org/x/sys/unix"
)

// descriptorMode reports whether an inherited descriptor is a pipe or a
// regular file without wrapping it. A wrapped descriptor is closed when the
// wrapper is collected, so one that is refused must never be wrapped: it
// belongs to whoever holds it.
func descriptorMode(fd int) (os.FileMode, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return 0, err
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFIFO:
		return os.ModeNamedPipe, nil
	case unix.S_IFREG:
		return 0, nil
	}
	return os.ModeIrregular, nil
}

// setCloseOnExec keeps an inherited descriptor out of everything this
// process starts. It fails on a descriptor that is not open.
func setCloseOnExec(fd int) error {
	_, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, unix.FD_CLOEXEC)
	return err
}
