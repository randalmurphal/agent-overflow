//go:build !linux

package procutil

import "os/exec"

// KillWithParent is a no-op where the kernel has no parent-death signal. On
// macOS the orphan reaper covers a group the caller registers with it.
func KillWithParent(*exec.Cmd) {}
