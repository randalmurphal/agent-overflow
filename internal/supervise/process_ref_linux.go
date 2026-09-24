//go:build linux

package supervise

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"time"
)

// processStart reads pid's start time (field 22 of /proc/<pid>/stat, clock
// ticks since boot) and whether it has not exited. A zombie has exited.
func processStart(pid int) (start string, alive bool, err error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read process %d: %w", pid, err)
	}
	// comm may hold spaces and parentheses; the fields after its last ')'
	// start at field 3 (state), so starttime is the 20th of them.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return "", false, fmt.Errorf("process %d stat has no command terminator", pid)
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 {
		return "", false, fmt.Errorf("process %d stat has %d fields, want its start time", pid, len(fields))
	}
	switch fields[0] {
	case "Z", "X", "x":
		return fields[19], false, nil
	}
	return fields[19], true, nil
}

func waitForExit(ctx context.Context, r ProcessRef, timeout time.Duration) error {
	return pollForExit(ctx, r, timeout)
}
