//go:build linux

package browser

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"agent-overflow/internal/procutil"
)

// configureChromiumProcess puts Chromium in a process group of its own and
// has the kernel kill it when the thread that started it exits, which for
// this process means when the backend dies, so a backend that crashes or is
// killed leaves no browser behind. The Go runtime ends a thread only when a
// goroutine exits while locked to it, and the launch runs on an ordinary
// goroutine.
func configureChromiumProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}

// awaitExit returns once the child pid has exited, leaving it unreaped.
func awaitExit(pid int) error {
	for {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

// killChromium kills every process in Chromium's group.
func killChromium(cmd *exec.Cmd) error { return procutil.KillConfiguredGroup(cmd) }

// waitGroupExited returns once every process in the group pgid has exited,
// or an error naming those still running when timeout passes. It runs after
// the group was killed: a process forked while the kill is sent receives it
// too, so the group gains no members.
//
// Each member is watched through a pidfd, which becomes readable once the
// process and all its threads have exited, whether or not anything reaps it
// and whoever its parent is. A kernel older than 5.3 has no pidfd, and
// nothing else reports the exit of a process that is not this one's child,
// so there the members' state is read every few milliseconds instead.
func waitGroupExited(pgid int, timeout time.Duration) error {
	members, err := groupMembers(pgid)
	if err != nil {
		return err
	}
	watched := make([]unix.PollFd, 0, len(members))
	defer func() {
		for _, member := range watched {
			unix.Close(int(member.Fd))
		}
	}()
	for _, pid := range members {
		fd, err := openMember(pid, pgid)
		if errors.Is(err, unix.ENOSYS) {
			return pollGroupExited(pgid, members, timeout)
		}
		if err != nil {
			return err
		}
		if fd >= 0 {
			watched = append(watched, unix.PollFd{Fd: int32(fd), Events: unix.POLLIN})
		}
	}
	deadline := time.Now().Add(timeout)
	for len(watched) > 0 {
		wait := time.Until(deadline)
		if wait <= 0 {
			return groupStillRunning(pgid, timeout)
		}
		if _, err := unix.Poll(watched, int(wait.Milliseconds())+1); err != nil && !errors.Is(err, unix.EINTR) {
			return fmt.Errorf("wait for Chromium's process group (%d): %w", pgid, err)
		}
		running := watched[:0]
		for _, member := range watched {
			if member.Revents == 0 {
				running = append(running, member)
				continue
			}
			unix.Close(int(member.Fd))
		}
		watched = running
	}
	return nil
}

// openMember opens a pidfd on pid if it is still a member of group pgid,
// and returns -1 if the member is gone.
func openMember(pid, pgid int) (int, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if errors.Is(err, unix.ENOSYS) {
		return -1, err
	}
	if err != nil {
		// A member that is gone is refused with ESRCH, or with EINVAL while
		// its pid is still allocated: during its release, or while it
		// names a process group or session that outlives it.
		if group, exited, ok := readProcess(pid); !ok || exited || group != pgid {
			return -1, nil
		}
		return -1, fmt.Errorf("watch Chromium process %d: %w", pid, err)
	}
	// The pidfd pins the process it names. A process outside the group took
	// the pid of a member that is gone.
	if group, _, ok := readProcess(pid); !ok || group != pgid {
		unix.Close(fd)
		return -1, nil
	}
	return fd, nil
}

// pollGroupExited is waitGroupExited for a kernel without pidfd.
func pollGroupExited(pgid int, members []int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		running := members[:0]
		for _, pid := range members {
			if group, exited, ok := readProcess(pid); ok && group == pgid && !exited {
				running = append(running, pid)
			}
		}
		members = running
		if len(members) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return groupStillRunning(pgid, timeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func groupStillRunning(pgid int, timeout time.Duration) error {
	members, _ := groupMembers(pgid)
	running := members[:0]
	for _, pid := range members {
		if _, exited, ok := readProcess(pid); ok && !exited {
			running = append(running, pid)
		}
	}
	return fmt.Errorf("Chromium processes %v were still running %s after their group (%d) was killed", running, timeout, pgid)
}

// groupMembers lists the processes in group pgid, exited or not.
func groupMembers(pgid int) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("list processes: %w", err)
	}
	var members []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if group, _, ok := readProcess(pid); ok && group == pgid {
			members = append(members, pid)
		}
	}
	return members, nil
}

// readProcess reads pid's process group and whether it has exited, from
// /proc. ok is false when no process has the pid.
func readProcess(pid int) (group int, exited, ok bool) {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false, false
	}
	return parseProcStat(stat)
}

// parseProcStat reads a /proc/<pid>/stat line. A zombie has exited once no
// other thread of it is still exiting: the first thread to finish turns the
// process into a zombie, and the last closes the files they share.
func parseProcStat(stat []byte) (group int, exited, ok bool) {
	// The command name is parenthesized and may hold spaces and
	// parentheses, so fields are counted from the last ")": the state is
	// the first, the group the third and the thread count the eighteenth.
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 {
		return 0, false, false
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) < 18 {
		return 0, false, false
	}
	group, err := strconv.Atoi(fields[2])
	if err != nil {
		return 0, false, false
	}
	threads, err := strconv.Atoi(fields[17])
	if err != nil {
		return 0, false, false
	}
	return group, (fields[0] == "Z" || fields[0] == "X") && threads <= 1, true
}
