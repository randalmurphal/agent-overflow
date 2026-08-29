//go:build windows

package procutil

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
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

// KillConfiguredGroup uses taskkill's tree walk for commands launched by the
// Windows-side harness CLI. The launcher itself uses a Job Object, but an
// ordinary functional-flow child is owned by this process directly.
func KillConfiguredGroup(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return fmt.Errorf("process is not running")
	}
	if command.ProcessState != nil {
		return os.ErrProcessDone
	}
	kill := exec.Command("taskkill.exe", "/PID", strconv.Itoa(command.Process.Pid), "/T", "/F")
	if out, err := kill.CombinedOutput(); err != nil {
		return fmt.Errorf("taskkill process tree %d: %w (%s)", command.Process.Pid, err, string(out))
	}
	return nil
}
