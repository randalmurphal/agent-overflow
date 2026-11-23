//go:build windows

package orphanreaper

import (
	"errors"
	"os/exec"

	"agent-overflow/internal/harness/instanceinfo"
)

// applySidecarProcessAttrs has nothing to set: the app starts no sidecar on
// Windows, whose backend runs in WSL.
func applySidecarProcessAttrs(cmd *exec.Cmd) {}

// readProcess refuses on Windows, where nothing records or sweeps a group.
func readProcess(int) (instanceinfo.Process, bool, error) {
	return instanceinfo.Process{}, false, errors.New("process groups are not recorded on windows")
}
