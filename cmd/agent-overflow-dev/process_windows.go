//go:build windows

package main

import "os/exec"

func platformSupportsProcessGroups() bool {
	return false
}

func prepareCommand(cmd *exec.Cmd) {}

func terminateProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}

func killProcessGroup(cmd *exec.Cmd) {
	terminateProcessGroup(cmd)
}
