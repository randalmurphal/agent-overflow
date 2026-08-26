//go:build windows

package instanceinfo

import (
	"errors"
	"syscall"
)

// ProcessAlive reports whether pid names a live process. Windows has no
// signal-0 probe, so this opens the process and asks for its exit code:
// STILL_ACTIVE (259) means running, anything else means the handle
// refers to a corpse that has not been reaped yet.
//
// A pid we may not open counts as alive for the same reason EPERM does
// on unix — the refusal proves the process exists.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	const stillActive = 259
	handle, err := syscall.OpenProcess(syscall.PROCESS_QUERY_INFORMATION, false, uint32(pid))
	if err != nil {
		// ERROR_ACCESS_DENIED means it is there and not ours; every other
		// failure (ERROR_INVALID_PARAMETER for an unknown pid) means gone.
		return errors.Is(err, syscall.ERROR_ACCESS_DENIED)
	}
	defer syscall.CloseHandle(handle)
	var code uint32
	if err := syscall.GetExitCodeProcess(handle, &code); err != nil {
		return false
	}
	return code == stillActive
}
