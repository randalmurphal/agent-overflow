//go:build darwin

package harnessclient

import (
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// awaitExitUnreaped blocks until child pid exits. A kqueue exit note does
// not reap, so the zombie holds its PID and group id until the launcher's
// cmd.Wait. A child that already exited is refused with ESRCH and is
// still unreaped.
func awaitExitUnreaped(pid int) error {
	kq, err := unix.Kqueue()
	if err != nil {
		return fmt.Errorf("watch backend %d exit: kqueue: %w", pid, err)
	}
	defer unix.Close(kq)
	change := []unix.Kevent_t{{
		Ident:  uint64(pid),
		Filter: unix.EVFILT_PROC,
		Flags:  unix.EV_ADD | unix.EV_ONESHOT,
		Fflags: unix.NOTE_EXIT,
	}}
	events := make([]unix.Kevent_t, 1)
	for {
		n, err := unix.Kevent(kq, change, events, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.ESRCH) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("watch backend %d exit: %w", pid, err)
		}
		if n == 0 {
			continue
		}
		if events[0].Flags&unix.EV_ERROR != 0 {
			if errno := syscall.Errno(events[0].Data); errno != unix.ESRCH {
				return fmt.Errorf("watch backend %d exit: %w", pid, errno)
			}
		}
		return nil
	}
}
