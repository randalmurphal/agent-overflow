package terminal

import (
	"os"

	"github.com/creack/pty"
)

// osFilePty bundles the PTY master *os.File with a typed resize method.
// Separating resize from the plain *os.File surface keeps the Setsize ioctl
// out of arbitrary call sites that only need read/write/close.
type osFilePty struct {
	*os.File
}

func (o *osFilePty) resize(rows, cols uint16) error {
	return pty.Setsize(o.File, &pty.Winsize{Rows: rows, Cols: cols})
}
