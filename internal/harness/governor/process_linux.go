//go:build linux

package governor

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"agent-overflow/internal/procrss"
)

type linuxProcesses struct{}

func (linuxProcesses) State(pid int) (ProcessState, error) {
	if pid <= 0 {
		return ProcessState{}, nil
	}
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		if os.IsNotExist(err) {
			return ProcessState{}, nil
		}
		return ProcessState{}, fmt.Errorf("harness governor: read process %d identity: %w", pid, err)
	}
	_, _, ok := procrss.ParseStat(data)
	if !ok {
		return ProcessState{}, fmt.Errorf("harness governor: parse process %d identity", pid)
	}
	// Field 22 is starttime. ParseStat intentionally exposes only the parent
	// map fields, so scan the stat tail after the final comm parenthesis.
	close := strings.LastIndexByte(string(data), ')')
	if close < 0 {
		return ProcessState{}, fmt.Errorf("harness governor: parse process %d identity", pid)
	}
	fields := strings.Fields(string(data[close+1:]))
	if len(fields) < 20 {
		return ProcessState{}, fmt.Errorf("harness governor: process %d stat is truncated", pid)
	}
	return ProcessState{Alive: true, BirthID: fields[19]}, nil
}

func (linuxProcesses) RSS(pid int) (uint64, error) {
	tree, err := procrss.SampleAll(pid)
	if err != nil {
		return 0, fmt.Errorf("harness governor: sample process %d memory: %w", pid, err)
	}
	return tree.TotalRSSBytes(), nil
}

func defaultProcesses() ProcessReader           { return linuxProcesses{} }
func defaultProcessMemory() ProcessMemoryReader { return linuxProcesses{} }
