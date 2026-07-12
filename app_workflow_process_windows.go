//go:build windows

package main

import (
	"os/exec"
	"time"
)

// Workflow providers and hooks execute in the Linux backend on Windows; the
// Windows binary is only the WSL launcher and never reaches this command path.
func configureWorkflowSetupCommand(command *exec.Cmd) {
	command.WaitDelay = time.Second
}
