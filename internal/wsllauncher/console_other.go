//go:build !windows

package wsllauncher

import "os/exec"

// hideConsole has nothing to hide off Windows.
func hideConsole(*exec.Cmd) {}
