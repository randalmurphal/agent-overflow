//go:build linux

package netisolate

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// A run's temp directory is a tmpfs when the host can afford it at launch,
// and otherwise a per-run directory on disk (diskTempPrefix) mounted at the
// same TempDir, created, removed and swept like the Go work directory.
//
// tempDirBytes caps the tmpfs, so a runaway test fails with ENOSPC instead
// of filling the host's memory; pages are allocated as they are written and
// the cap reserves nothing. It is over three times the largest peak
// measured on 24 cores, 1.2 GiB both for ./internal/store alone and for
// the whole tree (./internal/app: 0.3 GiB), which grows with the core
// count. ramTempReserve is what MemAvailable must hold beyond that cap:
// the developer's own processes and the memory the run's compilers, test
// binaries and browsers take besides temp files, for which the e2e gate's
// 8 GiB memory ceiling is the largest any one run is given. MemAvailable
// already leaves out what earlier runs hold in their tmpfs, so each launch
// sees the others' use. A run that starts below the threshold could not
// fill its cap without pushing the host toward swap, so it writes to disk.
const (
	tempDirBytes     = 4 << 30
	ramTempReserve   = 8 << 30
	ramTempThreshold = tempDirBytes + ramTempReserve
	// diskTempPrefix names a run's temp directory on disk.
	diskTempPrefix = "ao-tmp-"
)

// readMemAvailable returns the host's MemAvailable in bytes. Tests stand
// in for the host through it.
var readMemAvailable = func() (uint64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	return parseMemAvailable(data)
}

// ramTempAffordable decides whether a run may use a RAM temp directory,
// given the host's MemAvailable or the error reading it. When it may not,
// reason says why. An unreadable value means disk: nothing then shows the
// host can hold the cap, and disk costs only speed.
func ramTempAffordable(available uint64, readErr error) (ram bool, reason string) {
	if readErr != nil {
		return false, fmt.Sprintf("the host's available memory is unknown (%v)", readErr)
	}
	if available < ramTempThreshold {
		return false, fmt.Sprintf("the host has %s of memory available, under the %s a RAM one needs (its %s cap plus %s for everything else)",
			gib(available), gib(ramTempThreshold), gib(tempDirBytes), gib(ramTempReserve))
	}
	return true, ""
}

func gib(n uint64) string { return strconv.FormatFloat(float64(n)/(1<<30), 'f', 1, 64) + " GiB" }

// parseMemAvailable reads the MemAvailable line of /proc/meminfo.
func parseMemAvailable(meminfo []byte) (uint64, error) {
	scanner := bufio.NewScanner(bytes.NewReader(meminfo))
	for scanner.Scan() {
		value, ok := strings.CutPrefix(scanner.Text(), "MemAvailable:")
		if !ok {
			continue
		}
		kB, ok := strings.CutSuffix(strings.TrimSpace(value), " kB")
		if !ok {
			return 0, fmt.Errorf("MemAvailable %q is not in kB", strings.TrimSpace(value))
		}
		n, err := strconv.ParseUint(kB, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("MemAvailable: %w", err)
		}
		return n << 10, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, errors.New("no MemAvailable in /proc/meminfo")
}
