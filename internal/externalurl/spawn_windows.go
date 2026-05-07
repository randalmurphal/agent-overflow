//go:build windows

package externalurl

import (
	"os/exec"
	"syscall"
)

func applyDetachAttrs(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= 0x00000008 // DETACHED_PROCESS
}
