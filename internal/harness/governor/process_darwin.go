//go:build darwin

package governor

import (
	"fmt"
	"strconv"

	"agent-overflow/internal/procrss"

	"golang.org/x/sys/unix"
)

type darwinProcesses struct{}

func (darwinProcesses) State(pid int) (ProcessState, error) {
	if pid <= 0 {
		return ProcessState{}, nil
	}
	proc, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		// A pid that no longer exists answers ESRCH on some releases and,
		// in practice on current macOS, EIO: the sysctl itself succeeds
		// with ZERO bytes for a missing pid and SysctlKinfoProc reports
		// that short answer as EIO. Both mean "no such process". Reading
		// the second as a probe failure preserved every dead owner's
		// lease until TTL, which blocked `ao-harness up` for a day after
		// any instance died without releasing (observed 2026-09-04).
		if err == unix.ESRCH || err == unix.EIO {
			return ProcessState{}, nil
		}
		return ProcessState{}, fmt.Errorf("harness governor: identify process %d: %w", pid, err)
	}
	start := proc.Proc.P_starttime
	if start.Sec < 0 || start.Usec < 0 || start.Usec >= 1_000_000 {
		return ProcessState{}, fmt.Errorf("harness governor: parse process %d start time", pid)
	}
	return ProcessState{Alive: true, BirthID: strconv.FormatInt(start.Sec, 10) + "." + fmt.Sprintf("%06d", start.Usec)}, nil
}

func (darwinProcesses) RSS(pid int) (uint64, error) {
	tree, err := procrss.SampleAll(pid)
	if err != nil {
		return 0, fmt.Errorf("harness governor: sample Darwin application memory: %w", err)
	}
	return tree.TotalRSSBytes(), nil
}

func defaultProcesses() ProcessReader           { return darwinProcesses{} }
func defaultProcessMemory() ProcessMemoryReader { return darwinProcesses{} }
