//go:build linux

package harnessclient

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func processGroupHasLiveMember(pgid int) bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return true
	}
	seen := false
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 || !entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if err != nil {
			continue
		}
		close := strings.LastIndexByte(string(data), ')')
		if close < 0 || close+1 >= len(data) {
			continue
		}
		fields := strings.Fields(string(data[close+1:]))
		if len(fields) < 3 {
			continue
		}
		memberGroup, err := strconv.Atoi(fields[2])
		if err != nil || memberGroup != pgid {
			continue
		}
		seen = true
		switch fields[0] {
		case "Z", "X", "x":
			continue
		default:
			return true
		}
	}
	// If procfs was unreadable or the group vanished during the walk, retain
	// the signal-0 answer rather than claiming an unknown group is gone.
	return !seen
}
