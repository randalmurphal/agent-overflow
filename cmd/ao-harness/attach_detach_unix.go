//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// applyAttachDetachAttrs gives a `--detach`ed browser its own session so
// it survives this CLI returning. It runs AFTER procutil.ConfigureGroup,
// which owns the group-kill contract: Setsid makes the new process the
// leader of a new group whose pgid is its pid, which is exactly what
// ConfigureGroup's `kill(-pid)` cancel targets, so the teardown path
// stays the shared primitive rather than a second one.
func applyAttachDetachAttrs(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = false
	cmd.SysProcAttr.Setsid = true
}
