//go:build darwin || dragonfly || freebsd || netbsd || openbsd || solaris

package harnessclient

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Unix hosts without Linux's /proc source use the portable ps listing. It
// reports only membership; identity is captured through instanceinfo
// immediately afterwards.
func processGroupListing() ([]processGroupListingEntry, error) {
	out, err := exec.Command("ps", "-axo", "pid=,pgid=").Output()
	if err != nil {
		return nil, fmt.Errorf("list process groups: %w", err)
	}
	return parseProcessGroupListing(string(out))
}

func parseProcessGroupListing(text string) ([]processGroupListingEntry, error) {
	var entries []processGroupListingEntry
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			if strings.TrimSpace(line) == "" {
				continue
			}
			return nil, fmt.Errorf("invalid process-group listing line %q", line)
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 {
			return nil, fmt.Errorf("invalid process-group pid %q", fields[0])
		}
		pgid, err := strconv.Atoi(fields[1])
		if err != nil || pgid <= 0 {
			return nil, fmt.Errorf("invalid process-group id %q", fields[1])
		}
		entries = append(entries, processGroupListingEntry{pid: pid, pgid: pgid})
	}
	return entries, nil
}
