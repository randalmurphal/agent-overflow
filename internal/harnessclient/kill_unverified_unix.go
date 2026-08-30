//go:build unix

package harnessclient

import (
	"os"
	"os/exec"
	"syscall"
)

func killUnverifiedProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
