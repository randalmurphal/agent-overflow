//go:build darwin

package instanceinfo

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// ProcessIdentity is the OS evidence used for PID-safe lifecycle actions.
type ProcessIdentity struct {
	StartTime  string
	Executable string
	Namespace  string
}

// runProcessCommand is injectable in darwin tests. ps is part of macOS and
// supplies the human-readable executable marker. The lifecycle marker comes
// from the kernel process record below.
var runProcessCommand = func(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

func CaptureProcessIdentity(pid int) (ProcessIdentity, error) {
	if pid <= 0 {
		return ProcessIdentity{}, fmt.Errorf("process identity: %d is not a pid", pid)
	}
	out, err := runProcessCommand("/bin/ps", "-o", "comm=", "-p", strconv.Itoa(pid))
	if err != nil {
		return ProcessIdentity{}, fmt.Errorf("query process %d: %w", pid, err)
	}
	fields := strings.Fields(string(bytes.TrimSpace(out)))
	if len(fields) == 0 {
		return ProcessIdentity{}, fmt.Errorf("query process %d returned incomplete identity", pid)
	}
	proc, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return ProcessIdentity{}, fmt.Errorf("query process %d kernel identity: %w", pid, err)
	}
	start := proc.Proc.P_starttime
	if start.Sec < 0 || start.Usec < 0 || start.Usec >= 1_000_000 {
		return ProcessIdentity{}, fmt.Errorf("query process %d returned invalid start time", pid)
	}
	// ps comm is retained only as a stable human-readable executable marker.
	// The kernel start time is the lifecycle discriminator and has microsecond
	// precision, unlike ps lstart's one-second display.
	return ProcessIdentity{
		StartTime:  strconv.FormatInt(start.Sec, 10) + "." + fmt.Sprintf("%06d", start.Usec),
		Executable: filepath.Clean(strings.Join(fields, " ")),
		Namespace:  "darwin",
	}, nil
}

func CurrentPIDNamespace() string { return "darwin" }
