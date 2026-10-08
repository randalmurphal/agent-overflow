//go:build darwin

package browser

import (
	"errors"
	"fmt"
	"os/exec"
	"slices"
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

// exitingPollInterval is how often the state of a process that kqueue
// refused while it exits is read again.
const exitingPollInterval = 5 * time.Millisecond

// awaitExit returns once the child pid has exited, leaving it unreaped.
func awaitExit(pid int) error { return awaitChildExit(pid, notifyOnExit) }

// awaitChildExit is awaitExit with the kqueue registration as a parameter,
// so a test can hold the child in the state kqueue refuses. kqueue refuses a
// process from the moment it begins to exit until it is a zombie, so the
// state of a child that is already exiting is read every few milliseconds
// until it is one.
func awaitChildExit(pid int, notify func(kq, pid int) error) error {
	kq, err := unix.Kqueue()
	if err != nil {
		return err
	}
	defer unix.Close(kq)
	if err := notify(kq, pid); err != nil {
		if !errors.Is(err, unix.ESRCH) {
			return err
		}
		for !procutil.Exited(pid) {
			time.Sleep(exitingPollInterval)
		}
		return nil
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
// It refuses a member that has begun to exit and is not yet a zombie, for
// tens of milliseconds when the member has a large address space; that
// member may still be closing its files, so its state is read every few
// milliseconds until it is a zombie.
func waitGroupExited(pgid int, timeout time.Duration) error {
	return waitMembersExited(pgid, timeout, notifyOnExit)
}

// waitMembersExited is waitGroupExited with the kqueue registration as a
// parameter, so a test can hold a member in the state kqueue refuses.
func waitMembersExited(pgid int, timeout time.Duration, notify func(kq, pid int) error) error {
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
	var exiting []int
	for _, pid := range members {
		if err := notify(kq, pid); err != nil {
			if !procutil.RunningInGroup(pid, pgid) {
				continue
			}
			if !errors.Is(err, unix.ESRCH) {
				return fmt.Errorf("watch Chromium process %d: %w", pid, err)
			}
			exiting = append(exiting, pid)
			continue
		}
		// The registration names whichever process has the pid now. A
		// process outside the group took the pid of a member that is gone.
		if procutil.RunningInGroup(pid, pgid) {
			waiting[uint64(pid)] = true
		}
	}
	deadline := time.Now().Add(timeout)
	events := make([]unix.Kevent_t, len(members)+1)
	for {
		exiting = slices.DeleteFunc(exiting, func(pid int) bool { return !procutil.RunningInGroup(pid, pgid) })
		if len(waiting) == 0 && len(exiting) == 0 {
			return nil
		}
		wait := time.Until(deadline)
		if wait <= 0 {
			return groupStillRunning(pgid, timeout)
		}
		if len(exiting) > 0 {
			wait = min(wait, exitingPollInterval)
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
