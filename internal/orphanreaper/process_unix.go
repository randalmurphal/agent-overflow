//go:build !windows

package orphanreaper

import (
	"os/exec"
	"syscall"

	"agent-overflow/internal/harness/instanceinfo"
)

func readProcess(pid int) (instanceinfo.Process, bool, error) {
	return instanceinfo.ReadProcess(pid)
}

// Keep the sidecar outside the backend's process group. E2E and harness
// teardown signal the backend group first. If that also killed the sidecar,
// the pipe EOF reaper could never finish killing the watched groups.
func applySidecarProcessAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
