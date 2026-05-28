//go:build linux

package provider

import (
	"os/exec"
	"syscall"
)

// applySysProcAttr enables Setpgid so the spawned provider gets its own
// process group. signalGroupPlatform later uses a negative-PID kill to
// reach every descendant — Claude / Codex spawn helper processes during
// a turn (subagents, MCP servers, etc), and a per-PID kill would leave
// those orphaned.
//
// Pdeathsig delivers SIGTERM to the child when the parent process dies.
// Without it, Setpgid keeps children alive after an ungraceful parent
// exit (crash, SIGKILL) because the kernel reparents them to init
// instead of propagating the signal across process groups. The child's
// stdin pipe closes in that scenario, but the Claude CLI doesn't exit on
// stdin EOF — it sits idle consuming ~288 MB RSS per orphan
// indefinitely. Pdeathsig is Linux-only (prctl PR_SET_PDEATHSIG); see
// process_darwin.go for the macOS gap.
func applySysProcAttr(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.SysProcAttr.Pdeathsig = syscall.SIGTERM
}
