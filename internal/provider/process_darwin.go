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
// macOS has no Pdeathsig analogue — it's a Linux prctl, and Darwin's
// SysProcAttr carries no parent-death-signal field (the field's absence
// is exactly why the old shared !windows file failed to compile here).
// On ungraceful app exit, app_orphan_reaper.go's control-pipe sidecar and
// durable startup sweep terminate these otherwise launchd-reparented groups.
// Graceful shutdown still signals the whole group directly through Stop.
func applySysProcAttr(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}
