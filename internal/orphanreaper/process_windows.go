//go:build windows

package orphanreaper

import "os/exec"

// applySidecarProcessAttrs has nothing to set: the app starts no sidecar on
// Windows, whose backend runs in WSL.
func applySidecarProcessAttrs(cmd *exec.Cmd) {}
