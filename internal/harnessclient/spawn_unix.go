//go:build !windows

package harnessclient

import (
	"os"
	"os/exec"
	"syscall"
)

// applyDetachAttrs gives the backend its own session so it survives the
// tool that started it: `ao-harness up` returns as soon as the instance
// is attachable, and the instance then runs for as long as the agent
// working against it needs.
func applyDetachAttrs(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}

// requestStop is the polite stop: SIGTERM, which the backend's shutdown
// handler turns into a graceful teardown (sessions stopped, discovery
// files withdrawn).
func requestStop(proc *os.Process) error {
	return proc.Signal(syscall.SIGTERM)
}

func requestKill(proc *os.Process) error {
	return proc.Kill()
}

func signalOwnedGroup(pid int, force bool) error {
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	return syscall.Kill(-pid, sig)
}

func requestKillProcessGroup(pid int) error { return syscall.Kill(-pid, syscall.SIGKILL) }
