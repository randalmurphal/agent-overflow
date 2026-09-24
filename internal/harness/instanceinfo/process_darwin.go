//go:build darwin

package instanceinfo

import (
	"bytes"
	"errors"
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
	start, _, err := kernelStart(pid)
	if err != nil {
		return ProcessIdentity{}, fmt.Errorf("query process %d kernel identity: %w", pid, err)
	}
	// ps comm is retained only as a stable human-readable executable marker.
	// The kernel start time is the lifecycle discriminator and has microsecond
	// precision, unlike ps lstart's one-second display.
	return ProcessIdentity{
		StartTime:  start,
		Executable: filepath.Clean(strings.Join(fields, " ")),
		Namespace:  "darwin",
	}, nil
}

// errNoProcess is a pid the kernel has no record of.
var errNoProcess = errors.New("no such process")

// ProcessStart reads pid's birth marker, as ProcessIdentity.StartTime holds
// it, and whether the process has not exited. A zombie has exited. A pid
// that names no process is not alive, with a nil error.
func ProcessStart(pid int) (start string, alive bool, err error) {
	if pid <= 0 {
		return "", false, fmt.Errorf("process identity: %d is not a pid", pid)
	}
	start, zombie, err := kernelStart(pid)
	if errors.Is(err, errNoProcess) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read process %d: %w", pid, err)
	}
	return start, !zombie, nil
}

// kernelStart reads the start time, as seconds.microseconds, and the zombie
// state from the kernel's process record.
func kernelStart(pid int) (start string, zombie bool, err error) {
	proc, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		// A missing pid answers ESRCH on some releases and, on current
		// macOS, zero bytes, which SysctlKinfoProc reports as EIO.
		if errors.Is(err, unix.ESRCH) || errors.Is(err, unix.EIO) {
			return "", false, errNoProcess
		}
		return "", false, err
	}
	t := proc.Proc.P_starttime
	if t.Sec < 0 || t.Usec < 0 || t.Usec >= 1_000_000 {
		return "", false, fmt.Errorf("process %d has an invalid start time", pid)
	}
	// SZOMB is 5 in <sys/proc.h>.
	return strconv.FormatInt(t.Sec, 10) + "." + fmt.Sprintf("%06d", t.Usec), proc.Proc.P_stat == 5, nil
}

func CurrentPIDNamespace() string { return "darwin" }
