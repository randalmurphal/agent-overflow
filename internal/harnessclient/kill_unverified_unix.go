//go:build unix

package harnessclient

import (
	"os"
	"os/exec"
	"syscall"

	"agent-overflow/internal/procutil"
)

func killUnverifiedProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	return procutil.SignalGroup(cmd.Process.Pid, syscall.SIGKILL)
}
