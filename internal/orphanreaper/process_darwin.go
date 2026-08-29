//go:build darwin

package orphanreaper

import (
	"os/exec"
	"syscall"
)

// Keep the sidecar outside the backend's process group. E2E and harness
// teardown signal the backend group first. If that also killed the sidecar,
// the pipe EOF reaper could never finish killing provider groups.
func applySidecarProcessAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
