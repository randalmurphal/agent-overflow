//go:build windows

package instanceinfo

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/windows"
)

// ProcessIdentity is the portable shape. Windows uses the process creation
// FILETIME as its birth marker and the canonical executable path. Both are
// queried through a process handle, so a recycled PID cannot match.
type ProcessIdentity struct {
	StartTime  string
	Executable string
	Namespace  string
}

func CaptureProcessIdentity(pid int) (ProcessIdentity, error) {
	if pid <= 0 {
		return ProcessIdentity{}, fmt.Errorf("process identity: %d is not a pid", pid)
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return ProcessIdentity{}, fmt.Errorf("open process %d: %w", pid, err)
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_PATH)
	sz := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &sz); err != nil {
		return ProcessIdentity{}, fmt.Errorf("query process %d executable: %w", pid, err)
	}
	start, _, err := HandleStart(pid, h)
	if err != nil {
		return ProcessIdentity{}, err
	}
	return ProcessIdentity{
		StartTime:  start,
		Executable: filepath.Clean(windows.UTF16ToString(buf[:sz])),
		Namespace:  "windows",
	}, nil
}

// ProcessStart reads pid's birth marker, as ProcessIdentity.StartTime holds
// it, and whether the process has not exited. A pid that names no process
// is not alive, with a nil error.
func ProcessStart(pid int) (start string, alive bool, err error) {
	if pid <= 0 {
		return "", false, fmt.Errorf("process identity: %d is not a pid", pid)
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("open process %d: %w", pid, err)
	}
	defer windows.CloseHandle(h)
	return HandleStart(pid, h)
}

// HandleStart is ProcessStart through a handle the caller holds, so a wait
// on that handle is on the process whose start it read.
func HandleStart(pid int, h windows.Handle) (start string, alive bool, err error) {
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return "", false, fmt.Errorf("query process %d creation time: %w", pid, err)
	}
	return strconv.FormatInt(created.Nanoseconds(), 10), exited.HighDateTime == 0 && exited.LowDateTime == 0, nil
}

func CurrentPIDNamespace() string { return "windows" }
