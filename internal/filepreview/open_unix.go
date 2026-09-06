//go:build !windows

package filepreview

import (
	"os"
	"syscall"
)

// A path can be replaced by a FIFO between validation and open. Nonblocking
// open lets the descriptor's regular-file check refuse it without hanging.
func openFile(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
