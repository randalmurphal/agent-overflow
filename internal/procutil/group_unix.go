//go:build !windows

package procutil

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// ConfigureGroup puts the command in its own process group and makes context
// cancellation kill that whole group. A setup hook or a check command routinely
// spawns children (`sh -c 'make … & wait'`); killing only the direct child
// leaves them holding the worktree open past the timeout that was supposed to
// end them.
//
// WaitDelay bounds how long Wait blocks on inherited pipes after the kill, so a
// grandchild that ignored SIGKILL delivery ordering cannot wedge the reaper.
func ConfigureGroup(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	command.WaitDelay = time.Second
}
