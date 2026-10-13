//go:build linux

package procutil

import (
	"os/exec"
	"syscall"
)

// KillWithParent makes the kernel SIGKILL the command when this process
// dies, whatever the cause, including a SIGKILL that runs no cleanup. Call it
// after ConfigureGroup. It reaches the command itself, not descendants that
// outlive it.
func KillWithParent(command *exec.Cmd) {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.Pdeathsig = syscall.SIGKILL
}
