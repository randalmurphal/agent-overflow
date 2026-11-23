package main

import (
	"errors"
	"fmt"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// adoptOrphans does nothing: macOS has no subreaper, so a process that
// leaves the job's process group, as one a test starts in a group of its
// own, is neither signaled nor waited for there.
func adoptOrphans() error { return nil }

// szomb is a zombie's p_stat (SZOMB in <sys/proc.h>).
const szomb = 5

// endTree ends what is left in group, the process group of a job whose `go
// test`, its leader, has exited and been waited for, and returns once the
// group is empty: SIGTERM once, then SIGKILL once grace has passed. It
// returns the processes the group held before the SIGTERM, each by its pid
// and command name, which is all macOS lists for another process cheaply.
func endTree(group int, grace time.Duration) (left []leftover, err error) {
	procs, listErr := unix.SysctlKinfoProcSlice("kern.proc.pgrp", group)
	if listErr != nil {
		listErr = fmt.Errorf("list the group: %w", listErr)
	}
	for _, p := range procs {
		if p.Proc.P_stat != szomb {
			left = append(left, leftover{pid: int(p.Proc.P_pid), command: unix.ByteSliceToString(p.Proc.P_comm[:])})
		}
	}
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
			return left, listErr
		}
		if err != nil {
			return left, errors.Join(listErr, fmt.Errorf("signal the group: %w", err))
		}
		if sig != 0 {
			sent = sig
		}
		time.Sleep(10 * time.Millisecond)
	}
}
