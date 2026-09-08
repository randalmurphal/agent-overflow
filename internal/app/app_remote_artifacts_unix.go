//go:build !windows

package app

import (
	"os"
	"syscall"
)

// Validate the opened descriptor without a FIFO swap blocking the open itself.
func openRemoteArtifact(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
