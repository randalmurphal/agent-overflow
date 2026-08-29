//go:build windows

package harnessclient

import (
	"os"
	"os/exec"
	"syscall"
)

// applyDetachAttrs starts the backend outside this console so it
// survives the tool that launched it.
func applyDetachAttrs(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	// CREATE_NEW_PROCESS_GROUP gives the fallback task-tree terminator a
	// boundary even when the launch is attached. DETACHED_PROCESS is only
	// needed when the caller itself is returning.
	cmd.SysProcAttr.CreationFlags |= 0x00000200 // CREATE_NEW_PROCESS_GROUP
	if cmd.Stdout != nil || cmd.Stderr != nil {
		// A detached launch redirects both streams to files. Do not detach
		// an attached launch from its caller's console by accident.
		cmd.SysProcAttr.CreationFlags |= 0x00000008 // DETACHED_PROCESS
	}
}

// requestStop is a kill on Windows: there is no SIGTERM, and a detached
// process shares no console group to send a Ctrl-Break into. The cost is
// the instance's discovery files surviving as stale rows, which every
// reader already handles — a dead pid is stale by definition. Windows
// harness instances are normally hosted by the WSL launcher anyway
// (docs/specs/testing-harness.md §1), so this is the corner case, not
// the path.
func requestStop(proc *os.Process) error {
	return proc.Kill()
}

func requestKill(proc *os.Process) error { return proc.Kill() }
