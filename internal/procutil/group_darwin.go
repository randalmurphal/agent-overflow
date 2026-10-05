//go:build darwin

package procutil

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// darwinZombie is SZOMB from <sys/proc.h>, a process that has exited and
// waits to be reaped.
const darwinZombie = 5

// RunningGroupMembers lists the processes in group pgid that have not
// exited.
func RunningGroupMembers(pgid int) ([]int, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
	if err != nil {
		return nil, fmt.Errorf("list process group %d: %w", pgid, err)
	}
	var members []int
	for _, proc := range procs {
		if proc.Proc.P_stat != darwinZombie {
			members = append(members, int(proc.Proc.P_pid))
		}
	}
	return members, nil
}

// RunningInGroup reports whether pid is a process in group pgid that has
// not exited.
func RunningInGroup(pid, pgid int) bool {
	proc, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	return err == nil && int(proc.Eproc.Pgid) == pgid && proc.Proc.P_stat != darwinZombie
}

// Exited reports whether pid has exited but is not yet reaped (a zombie).
// Signal 0 still succeeds for it. A process whose state cannot be read is
// not known to have exited.
func Exited(pid int) bool {
	proc, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	return err == nil && proc.Proc.P_pid == int32(pid) && proc.Proc.P_stat == darwinZombie
}

// groupExited reports whether no process in group pgid can still run. A
// group that cannot be listed is not known to have exited.
func groupExited(pgid int) bool {
	members, err := RunningGroupMembers(pgid)
	return err == nil && len(members) == 0
}
