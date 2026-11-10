//go:build linux

package harnessclient

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// awaitExitUnreaped blocks until child pid exits and leaves it unreaped:
// WNOWAIT keeps the zombie, which holds its PID and group id until the
// launcher's cmd.Wait.
func awaitExitUnreaped(pid int) error {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("wait for backend %d to exit: %w", pid, err)
		}
		return nil
	}
}
