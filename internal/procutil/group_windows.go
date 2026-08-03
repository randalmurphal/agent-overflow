//go:build windows

package procutil

import (
	"os/exec"
	"time"
)

// ConfigureGroup is the Windows counterpart. Workflow providers, check
// commands, and worktree setup hooks all execute in the Linux backend on
// Windows — the Windows binary is only the WSL launcher and never reaches this
// command path — so there is no Job Object to attach here. WaitDelay still
// bounds the reaper.
func ConfigureGroup(command *exec.Cmd) {
	command.WaitDelay = time.Second
}
