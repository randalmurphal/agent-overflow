//go:build windows

package harnessclient

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
)

func killUnverifiedProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	kill := exec.Command("taskkill.exe", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F")
	if out, err := kill.CombinedOutput(); err != nil {
		return fmt.Errorf("taskkill process tree %d: %w (%s)", cmd.Process.Pid, err, string(out))
	}
	return nil
}
