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
		if err == unix.ESRCH {
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
