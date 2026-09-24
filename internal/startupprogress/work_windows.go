//go:build windows

package startupprogress

import (
	"fmt"
	"syscall"
	"time"
	"unsafe"
)

// Stdlib-only, as the package is: syscall carries GetProcessTimes, and
// GetProcessIoCounters is loaded from kernel32.
var procGetProcessIoCounters = syscall.NewLazyDLL("kernel32.dll").NewProc("GetProcessIoCounters")

// ioCounters is IO_COUNTERS.
type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

// ReadProcessWork samples this process's CPU time (GetProcessTimes) and
// the bytes its reads and writes transferred (GetProcessIoCounters). The
// transfer counts include socket I/O; answering readiness polls stays far
// under the I/O threshold.
func ReadProcessWork() (ProcessWork, error) {
	self, err := syscall.GetCurrentProcess()
	if err != nil {
		return ProcessWork{}, fmt.Errorf("GetCurrentProcess: %w", err)
	}
	var created, exited, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(self, &created, &exited, &kernel, &user); err != nil {
		return ProcessWork{}, fmt.Errorf("GetProcessTimes: %w", err)
	}
	var counters ioCounters
	if r, _, err := procGetProcessIoCounters.Call(uintptr(self), uintptr(unsafe.Pointer(&counters))); r == 0 {
		return ProcessWork{}, fmt.Errorf("GetProcessIoCounters: %w", err)
	}
	// FILETIME durations count 100 ns intervals.
	cpu := time.Duration(filetimeTicks(kernel)+filetimeTicks(user)) * 100
	return ProcessWork{
		CPU:      cpu,
		IO:       int64(counters.ReadTransferCount + counters.WriteTransferCount),
		IOSource: "GetProcessIoCounters",
	}, nil
}

func filetimeTicks(ft syscall.Filetime) int64 {
	return int64(ft.HighDateTime)<<32 | int64(ft.LowDateTime)
}
