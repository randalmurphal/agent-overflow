//go:build windows

package supervise

import (
	"os"

	"golang.org/x/sys/windows"
)

// descriptorMode reports whether an inherited handle is a pipe or a file
// without wrapping it, as the Unix version does.
func descriptorMode(fd int) (os.FileMode, error) {
	kind, err := windows.GetFileType(windows.Handle(fd))
	if err != nil {
		return 0, err
	}
	switch kind {
	case windows.FILE_TYPE_PIPE:
		return os.ModeNamedPipe, nil
	case windows.FILE_TYPE_DISK:
		return 0, nil
	}
	return os.ModeIrregular, nil
}

// setCloseOnExec has nothing to do where no child is spawned with inherited
// descriptors: supervise children are Unix processes.
func setCloseOnExec(int) error { return nil }
