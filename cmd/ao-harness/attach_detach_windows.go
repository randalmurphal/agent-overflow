//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// applyAttachDetachAttrs starts a `--detach`ed browser outside this
// console so it survives the CLI returning. Teardown stays
// procutil.KillConfiguredGroup, whose Windows build walks the task tree
// with taskkill /T — CREATE_NEW_PROCESS_GROUP only gives that walk a
// boundary.
func applyAttachDetachAttrs(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= 0x00000200 // CREATE_NEW_PROCESS_GROUP
	cmd.SysProcAttr.CreationFlags |= 0x00000008 // DETACHED_PROCESS
}
