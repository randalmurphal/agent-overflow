//go:build linux

package harnessclient

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func processGroupListing() ([]processGroupListingEntry, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("list /proc: %w", err)
	}
	result := make([]processGroupListingEntry, 0, len(entries)/8)
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
		// The suffix begins at field 3. pgrp is field 5, index 2.
		fields := strings.Fields(string(data[close+1:]))
		if len(fields) < 3 {
			continue
		}
		pgid, err := strconv.Atoi(fields[2])
		if err != nil || pgid <= 0 {
			continue
		}
		result = append(result, processGroupListingEntry{pid: pid, pgid: pgid})
	}
	return result, nil
}
