//go:build darwin

package governor

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"

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
	out, err := exec.Command("/bin/ps", "-axo", "pid=", "ppid=", "rss=").Output()
	if err != nil {
		return 0, fmt.Errorf("harness governor: enumerate process tree: %w", err)
	}
	type row struct{ parent, rss uint64 }
	rows := make(map[uint64]row)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		p, e1 := strconv.ParseUint(fields[0], 10, 64)
		parent, e2 := strconv.ParseUint(fields[1], 10, 64)
		rss, e3 := strconv.ParseUint(fields[2], 10, 64)
		if e1 == nil && e2 == nil && e3 == nil {
			rows[p] = row{parent: parent, rss: rss}
		}
	}
	root := uint64(pid)
	if _, ok := rows[root]; !ok {
		return 0, fmt.Errorf("harness governor: process %d disappeared during tree sample", pid)
	}
	var total uint64
	queue := []uint64{root}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if item, ok := rows[current]; ok {
			total += item.rss * 1024
		}
		for child, item := range rows {
			if item.parent == current && child != current {
				queue = append(queue, child)
			}
		}
	}
	return total, nil
}

func defaultProcesses() ProcessReader           { return darwinProcesses{} }
func defaultProcessMemory() ProcessMemoryReader { return darwinProcesses{} }
