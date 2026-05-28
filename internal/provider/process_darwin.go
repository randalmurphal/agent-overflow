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
// Consequence: on an ungraceful parent exit (crash, SIGKILL) the
// provider children are reparented to launchd and survive, because the
// Claude CLI ignores stdin EOF and lingers at ~288 MB RSS per orphan.
// Graceful shutdown is unaffected — Stop signals the whole group
// explicitly. Closing this gap would need a parent-death watchdog (the
// child polling getppid, or a wrapper process); that's a separate change
// and intentionally out of scope here.
func applySysProcAttr(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}
