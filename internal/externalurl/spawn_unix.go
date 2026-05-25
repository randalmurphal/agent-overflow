//go:build !windows

package externalurl

import (
	"os/exec"
	"syscall"
)

// applyDetachAttrs puts the browser opener in its own session via
// Setsid so it fully survives the parent process exiting — including
// the WSL distro teardown path where the Windows launcher's Job Object
// kills wsl.exe.
func applyDetachAttrs(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}
