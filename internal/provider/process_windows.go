//go:build windows

// Windows stub for the provider package's process-group helpers. The
// Windows binary (cmd/agent-overflow-windows) never spawns provider
// children — it shells out to wsl.exe and the WSL-side Linux backend
// owns provider lifecycle. These stubs exist purely so the
// GOOS=windows ./... cross-compile succeeds.
package provider

import (
	"os/exec"
	"syscall"
)

// applySysProcAttr is a no-op on Windows: there's no Setpgid analogue
// and provider children never spawn here. We deliberately don't reach
// for Job Objects in this stub — the launcher's Job Object (in
// internal/wsllauncher) is the right place for that lifecycle hook.
func applySysProcAttr(cmd *exec.Cmd) {
	_ = cmd
}

// signalGroupPlatform is a no-op on Windows. If a future port wires
// real provider spawning into the Windows binary, this is the seam to
// implement: open the process via OpenProcess + TerminateProcess, or
// adopt into a Job Object.
func signalGroupPlatform(pid int, sig syscall.Signal) {
	_ = pid
	_ = sig
}
