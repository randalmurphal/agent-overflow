package main

import (
	"errors"
	"fmt"
	"syscall"
	"time"
)

// adoptOrphans does nothing: macOS has no subreaper, so a process that
// leaves the job's process group, as one a test starts in a group of its
// own, is neither signaled nor waited for there.
func adoptOrphans() error { return nil }

// endTree ends what is left in group, the process group of a job whose `go
// test`, its leader, has exited and been waited for, and returns once the
// group is empty: SIGTERM once, then SIGKILL once grace has passed. It
// names what was left only as the group's processes, which macOS does not
// list cheaply.
func endTree(group int, grace time.Duration) (left []string, err error) {
	deadline := time.Now().Add(grace)
	var sent syscall.Signal
	for {
		var sig syscall.Signal // 0 asks whether the group has a process
		switch {
		case sent == 0:
			sig = syscall.SIGTERM
		case sent == syscall.SIGTERM && !time.Now().Before(deadline):
			sig = syscall.SIGKILL
		}
		err := syscall.Kill(-group, sig)
		if errors.Is(err, syscall.ESRCH) {
			return left, nil
		}
		if err != nil {
			return left, fmt.Errorf("signal the group: %w", err)
		}
		if sig != 0 {
			sent = sig
		}
		left = []string{"the processes of its process group"}
		time.Sleep(10 * time.Millisecond)
	}
}
