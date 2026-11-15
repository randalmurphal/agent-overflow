package procutil

import (
	"os/exec"
	"syscall"
)

// endWithParent has the kernel send the command's leader SIGTERM when the
// thread that started it exits. The Go runtime keeps its threads until the
// process ends unless a goroutine locked to one exits, and nothing that
// starts a group locks its thread. ConfigureGroup has set SysProcAttr.
func endWithParent(cmd *exec.Cmd) {
	cmd.SysProcAttr.Pdeathsig = syscall.SIGTERM
}
