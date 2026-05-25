//go:build !windows

package editor

import (
	"os"
	"os/exec"
	"syscall"
)

// applyDetachAttrs puts the editor in its own session via Setsid so it
// fully survives the parent process exiting. Setsid creates a new
// session AND a new process group, detaching from the parent's
// controlling terminal. This is safe because SysProcAttr.Setsid is
// applied in the forked child (which is never a session/group leader),
// so the setsid(2) syscall always succeeds.
func applyDetachAttrs(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}

// openDevNull returns an os.File pointing at /dev/null, opened
// read/write. Used as the editor's stdout/stderr so its log noise
// doesn't get attributed to our process.
func openDevNull() (*os.File, error) {
	return os.OpenFile(os.DevNull, os.O_RDWR, 0)
}
