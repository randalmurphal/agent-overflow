package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// adoptOrphans makes this process the subreaper of what it starts: a
// process whose parent exits comes to this one, not to init, so endTree
// finds and waits for it whatever its process group. endTree lists the
// children through /proc, which must offer that list.
func adoptOrphans() error {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("become a subreaper: %w", err)
	}
	if _, err := children(); err != nil {
		return err
	}
	return nil
}

// endTree ends what is left of a job whose `go test`, the leader of group,
// has exited and been waited for, and returns once no process of it is
// left: those still in its group, and every process that came to this one,
// the subreaper, from whatever group, as its parent exited. Each gets
// SIGTERM once and, once grace has passed, SIGKILL. It returns the
// processes that came to this one alive.
func endTree(group int, grace time.Duration) (left []leftover, err error) {
	if err := syscall.Kill(-group, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return nil, fmt.Errorf("signal the group: %w", err)
	}
	deadline := time.Now().Add(grace)
	killed := false
	signaled := map[int]bool{}
	for {
		none, err := reapExited()
		if err != nil || none {
			return left, err
		}
		sig := syscall.SIGTERM
		if !time.Now().Before(deadline) {
			sig = syscall.SIGKILL
			if !killed {
				killed = true
				clear(signaled)
				_ = syscall.Kill(-group, sig)
			}
		}
		pids, err := children()
		if err != nil {
			return left, err
		}
		for _, pid := range pids {
			if signaled[pid] {
				continue
			}
			if !killed {
				if line := commandLine(pid); line != "" {
					// A process that exited since leaves no directory.
					dir, _ := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "cwd"))
					left = append(left, leftover{pid: pid, command: line, dir: dir})
				}
			}
			signaled[pid] = true
			_ = syscall.Kill(pid, sig)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// reapExited waits for every child of this process that has exited and
// reports whether none is left.
func reapExited() (none bool, err error) {
	for {
		pid, err := syscall.Wait4(-1, nil, syscall.WNOHANG, nil)
		switch {
		case errors.Is(err, syscall.ECHILD):
			return true, nil
		case errors.Is(err, syscall.EINTR):
		case err != nil:
			return false, fmt.Errorf("wait for the processes it left: %w", err)
		case pid == 0:
			return false, nil
		}
	}
}

// children lists this process's children, from the list each of its
// threads keeps.
func children() ([]int, error) {
	tasks, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return nil, fmt.Errorf("list this process's threads: %w", err)
	}
	var pids []int
	for _, task := range tasks {
		data, err := os.ReadFile(filepath.Join("/proc/self/task", task.Name(), "children"))
		if errors.Is(err, os.ErrNotExist) {
			// A thread that exited since the list was read.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("list this process's children (/proc/self/task/*/children): %w", err)
		}
		for _, field := range strings.Fields(string(data)) {
			if pid, err := strconv.Atoi(field); err == nil {
				pids = append(pids, pid)
			}
		}
	}
	return pids, nil
}

// commandLine is pid's command line, or "" for a process that has exited.
func commandLine(pid int) string {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.ReplaceAll(string(data), "\x00", " "))
}
