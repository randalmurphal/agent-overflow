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
// parentDeathSignal is for a spawn the orphan reaper does not watch: its
// leader gets SIGTERM when the backend dies (Pdeathsig). The stdin pipe
// closes then too, but the Claude CLI does not exit on stdin EOF. A
// watched spawn is ended by the reaper (SpawnConfig.Reaper).
func applySysProcAttr(cmd *exec.Cmd, parentDeathSignal bool) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	if parentDeathSignal {
		cmd.SysProcAttr.Pdeathsig = syscall.SIGTERM
	}
}
