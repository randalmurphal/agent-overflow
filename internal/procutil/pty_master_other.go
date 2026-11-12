//go:build !windows && !linux

package procutil

import "os"

// PollableMaster keeps the master creack/pty returns, which stays in
// blocking mode here: kqueue does not take ptys on darwin. Closing it does
// not wake a read. The kernel ends the read instead when the session leader
// that has the pty as its terminal exits, which revokes the terminal, or
// when every holder of the other end has closed it.
func PollableMaster(f *os.File) (*os.File, error) {
	return f, nil
}
