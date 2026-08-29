//go:build !unix && !windows

package harnessclient

import (
	"os"
	"os/exec"
)

func killUnverifiedProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	return cmd.Process.Kill()
}
