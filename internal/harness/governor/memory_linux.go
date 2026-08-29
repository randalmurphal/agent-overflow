//go:build linux

package governor

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type linuxMemory struct{}

func (linuxMemory) AvailableMemory() (uint64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, fmt.Errorf("harness governor: open /proc/meminfo: %w", err)
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) >= 2 && fields[0] == "MemAvailable:" {
			kb, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("harness governor: parse MemAvailable: %w", err)
			}
			return kb * 1024, nil
		}
	}
	if err := s.Err(); err != nil {
		return 0, fmt.Errorf("harness governor: read /proc/meminfo: %w", err)
	}
	return 0, fmt.Errorf("harness governor: /proc/meminfo has no MemAvailable")
}

func defaultMemory() MemoryReader { return linuxMemory{} }
