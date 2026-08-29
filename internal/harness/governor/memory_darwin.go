//go:build darwin

package governor

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

var runVMStat = func() ([]byte, error) { return exec.Command("/usr/bin/vm_stat").Output() }

type darwinMemory struct{}

func (darwinMemory) AvailableMemory() (uint64, error) {
	out, err := runVMStat()
	if err != nil {
		return 0, fmt.Errorf("harness governor: vm_stat: %w", err)
	}
	pageSize := uint64(4096)
	var availablePages uint64
	var foundPages bool
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if n, ok := parseVMStatPageSize(line); ok {
			pageSize = n
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		name := strings.TrimSpace(line[:colon])
		if name != "Pages free" && name != "Pages inactive" && name != "Pages speculative" && name != "Pages purgeable" {
			continue
		}
		value := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line[colon+1:]), "."))
		value = strings.ReplaceAll(value, ".", "")
		n, parseErr := strconv.ParseUint(value, 10, 64)
		if parseErr != nil {
			return 0, fmt.Errorf("harness governor: parse vm_stat %q: %w", line, parseErr)
		}
		if ^uint64(0)-availablePages < n {
			return 0, fmt.Errorf("harness governor: vm_stat available pages overflow")
		}
		availablePages += n
		foundPages = true
	}
	if !foundPages {
		return 0, fmt.Errorf("harness governor: vm_stat returned no available pages")
	}
	if availablePages > ^uint64(0)/pageSize {
		return 0, fmt.Errorf("harness governor: vm_stat available bytes overflow")
	}
	return availablePages * pageSize, nil
}

// parseVMStatPageSize accepts both the current header and older versions of
// vm_stat. On Apple Silicon the header is
// "Mach Virtual Memory Statistics: (page size of 16384 bytes)". Parsing only
// a line prefix leaves the default 4096-byte page size in place and
// underreports available memory by 4x.
func parseVMStatPageSize(line string) (uint64, bool) {
	const marker = "page size of "
	start := strings.Index(line, marker)
	if start < 0 {
		return 0, false
	}
	fields := strings.Fields(line[start+len(marker):])
	if len(fields) == 0 {
		return 0, false
	}
	n, err := strconv.ParseUint(fields[0], 10, 64)
	return n, err == nil && n > 0
}

func defaultMemory() MemoryReader { return darwinMemory{} }
