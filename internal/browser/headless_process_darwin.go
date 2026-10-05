//go:build darwin

package browser

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"agent-overflow/internal/procutil"
)

// configureChromiumProcess puts Chromium in a process group of its own.
// macOS has no parent-death signal, so Dispose is what stops it.
func configureChromiumProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killChromium kills every process in Chromium's group.
func killChromium(cmd *exec.Cmd) error { return procutil.KillConfiguredGroup(cmd) }

// awaitExit returns once the child pid has exited, leaving it unreaped.
func awaitExit(pid int) error {
	kq, err := unix.Kqueue()
	if err != nil {
		return err
	}
	defer unix.Close(kq)
	if err := notifyOnExit(kq, pid); err != nil {
		if errors.Is(err, unix.ESRCH) {
			// kqueue refuses a process that has already exited.
			return nil
		}
		return err
	}
	events := make([]unix.Kevent_t, 1)
	for {
		if _, err := unix.Kevent(kq, nil, events, nil); !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

// waitGroupExited returns once every process in the group pgid has exited,
// or an error naming those still running when timeout passes. It runs after
// the group was killed, so the group gains no members. kqueue reports each
// member's exit whether or not anything reaps it and whoever its parent is.
func waitGroupExited(pgid int, timeout time.Duration) error {
	members, err := procutil.RunningGroupMembers(pgid)
	if err != nil {
		return err
	}
	kq, err := unix.Kqueue()
	if err != nil {
		return fmt.Errorf("wait for Chromium's process group (%d): %w", pgid, err)
	}
	defer unix.Close(kq)
	waiting := make(map[uint64]bool, len(members))
	for _, pid := range members {
		if err := notifyOnExit(kq, pid); err != nil {
			// kqueue refuses a process that has exited.
			if !procutil.RunningInGroup(pid, pgid) {
				continue
			}
			return fmt.Errorf("watch Chromium process %d: %w", pid, err)
		}
		// The registration names whichever process has the pid now. A
		// process outside the group took the pid of a member that is gone.
		if procutil.RunningInGroup(pid, pgid) {
			waiting[uint64(pid)] = true
		}
	}
	deadline := time.Now().Add(timeout)
	events := make([]unix.Kevent_t, len(members)+1)
	for len(waiting) > 0 {
		wait := time.Until(deadline)
		if wait <= 0 {
			return groupStillRunning(pgid, timeout)
		}
		timespec := unix.NsecToTimespec(wait.Nanoseconds())
		n, err := unix.Kevent(kq, nil, events, &timespec)
		if err != nil && !errors.Is(err, unix.EINTR) {
			return fmt.Errorf("wait for Chromium's process group (%d): %w", pgid, err)
		}
		for _, event := range events[:max(n, 0)] {
			delete(waiting, event.Ident)
		}
	}
	return nil
}

// notifyOnExit registers kq for pid's exit, once.
func notifyOnExit(kq, pid int) error {
	change := unix.Kevent_t{
		Ident:  uint64(pid),
		Filter: unix.EVFILT_PROC,
		Flags:  unix.EV_ADD | unix.EV_ONESHOT,
		Fflags: unix.NOTE_EXIT,
	}
	_, err := unix.Kevent(kq, []unix.Kevent_t{change}, nil, nil)
	return err
}

func groupStillRunning(pgid int, timeout time.Duration) error {
	members, _ := procutil.RunningGroupMembers(pgid)
	return fmt.Errorf("Chromium processes %v were still running %s after their group (%d) was killed", members, timeout, pgid)
}
