//go:build !linux && !windows

package browser

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// chromiumLeftovers lists the processes still running in Chromium's group,
// or naming anything under root in their command line.
func chromiumLeftovers(t *testing.T, group int, root string) []string {
	t.Helper()
	out, err := exec.Command("ps", "-A", "-o", "pid=", "-o", "pgid=", "-o", "stat=", "-o", "command=").Output()
	if err != nil {
		t.Fatalf("list processes: %v", err)
	}
	var left []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || strings.HasPrefix(fields[2], "Z") {
			continue
		}
		command := strings.Join(fields[3:], " ")
		if pgid, err := strconv.Atoi(fields[1]); err == nil && pgid == group {
			left = append(left, fmt.Sprintf("%s in the group: %s", fields[0], command))
		} else if strings.Contains(command, root) {
			left = append(left, fmt.Sprintf("%s names it: %s", fields[0], command))
		}
	}
	return left
}
