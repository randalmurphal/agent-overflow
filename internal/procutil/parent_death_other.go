//go:build !linux

package procutil

import "os/exec"

// endWithParent has nothing to set: only Linux has a parent-death signal.
// The caller records the group for the orphan reaper (GroupCommand.Started).
func endWithParent(*exec.Cmd) {}
