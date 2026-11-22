//go:build windows

package orphanreaper

import (
	"errors"
	"os"
	"os/exec"
)

// applySidecarProcessAttrs has nothing to set: the app starts no sidecar on
// Windows, whose backend runs in WSL.
func applySidecarProcessAttrs(cmd *exec.Cmd) {}

// writeWithoutWaiting exists so Spawn compiles on Windows, where the app
// starts no sidecar.
func writeWithoutWaiting(w *os.File, data string) (int, error) {
	return 0, errors.New("orphanreaper: the app starts no sidecar on Windows")
}
