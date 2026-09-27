//go:build !windows && !linux

package terminal

import "os"

// pollableMaster keeps the master creack/pty returns, which stays in
// blocking mode here: kqueue does not take ptys on darwin. The output
// pump's read needs no wake when the shell exits, because the kernel
// revokes the terminal of a session leader that exits, which ends the read.
func pollableMaster(f *os.File) (*os.File, error) {
	return f, nil
}
