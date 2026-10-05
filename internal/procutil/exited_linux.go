package procutil

import (
	"os"
	"strconv"
	"strings"
)

// Exited reports whether pid has exited but is not yet reaped (a zombie).
// Signal 0 still succeeds for it. A process whose state cannot be read is
// not known to have exited.
func Exited(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return false
	}
	fields := strings.Fields(string(data)[end+1:])
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "Z", "X", "x":
		return true
	}
	return false
}
