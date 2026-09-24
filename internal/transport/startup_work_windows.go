//go:build windows

package transport

import (
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procGetProcessIoCounters = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetProcessIoCounters")

// readProcessWork samples this process's CPU time (GetProcessTimes) and
// the bytes its reads and writes transferred (GetProcessIoCounters). The
// transfer counts include socket I/O; answering readiness polls stays far
// under the I/O threshold.
func readProcessWork() (processWork, error) {
	self := windows.CurrentProcess()
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(self, &created, &exited, &kernel, &user); err != nil {
		return processWork{}, fmt.Errorf("GetProcessTimes: %w", err)
	}
	var counters windows.IO_COUNTERS
	if r, _, err := procGetProcessIoCounters.Call(uintptr(self), uintptr(unsafe.Pointer(&counters))); r == 0 {
		return processWork{}, fmt.Errorf("GetProcessIoCounters: %w", err)
	}
	// FILETIME durations count 100 ns intervals.
	cpu := time.Duration(filetimeTicks(kernel)+filetimeTicks(user)) * 100
	return processWork{
		cpu:      cpu,
		io:       int64(counters.ReadTransferCount + counters.WriteTransferCount),
		ioSource: "GetProcessIoCounters",
	}, nil
}

func filetimeTicks(ft windows.Filetime) int64 {
	return int64(ft.HighDateTime)<<32 | int64(ft.LowDateTime)
}
