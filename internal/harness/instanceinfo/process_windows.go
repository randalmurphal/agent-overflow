//go:build windows

package instanceinfo

import (
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
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return ProcessIdentity{}, fmt.Errorf("query process %d creation time: %w", pid, err)
	}
	return ProcessIdentity{
		StartTime:  strconv.FormatInt(created.Nanoseconds(), 10),
		Executable: filepath.Clean(windows.UTF16ToString(buf[:sz])),
		Namespace:  "windows",
	}, nil
}

func CurrentPIDNamespace() string { return "windows" }
