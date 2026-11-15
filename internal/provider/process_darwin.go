//go:build darwin

package provider

import (
	"os/exec"
	"syscall"
)

// applySysProcAttr enables Setpgid so the spawned provider gets its own
// process group; signalGroupPlatform's negative-PID kill then reaches
// every descendant when Stop tears the session down.
//
// macOS has no parent-death signal, so parentDeathSignal has nothing to
// set: only a spawn handed to the orphan reaper (SpawnConfig.Reaper) ends
// with the backend here.
func applySysProcAttr(cmd *exec.Cmd, parentDeathSignal bool) {
	_ = parentDeathSignal
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}
