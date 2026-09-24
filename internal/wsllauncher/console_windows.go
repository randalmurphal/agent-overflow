//go:build windows

package wsllauncher

import (
	"os/exec"
	"syscall"
)

// hideConsole keeps a wsl.exe started from the GUI launcher from flashing a
// console window.
func hideConsole(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
}
