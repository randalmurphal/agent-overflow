//go:build linux

package instanceinfo

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ProcessIdentity is the OS evidence used when a lifecycle command cannot
// use the authenticated shutdown RPC. PID is intentionally not part of it.
type ProcessIdentity struct {
	StartTime  string
	Executable string
	Namespace  string
}

// CaptureProcessIdentity reads the process birth marker and executable from
// procfs. Linux starttime is monotonic for a PID, so a recycled PID cannot
// satisfy the same record. Missing procfs evidence is an error rather than a
// wildcard match.
func CaptureProcessIdentity(pid int) (ProcessIdentity, error) {
	if pid <= 0 {
		return ProcessIdentity{}, fmt.Errorf("process identity: %d is not a pid", pid)
	}
	base := filepath.Join("/proc", fmt.Sprint(pid))
	_, start, err := readStat(pid)
	if err != nil {
		return ProcessIdentity{}, err
	}
	exe, err := os.Readlink(filepath.Join(base, "exe"))
	if err != nil {
		return ProcessIdentity{}, fmt.Errorf("read process %d executable: %w", pid, err)
	}
	// When the binary is replaced on disk (every `make harness-build`
	// over a running rig), the kernel appends " (deleted)" to the link
	// target. That marker is procfs metadata, not part of the path, and
	// leaving it in made `ao-harness down` refuse the very process it
	// recorded at spawn. StartTime stays the anti-recycling evidence.
	exe = strings.TrimSuffix(exe, " (deleted)")
	procNS, err := os.Readlink(filepath.Join(base, "ns", "pid"))
	if err != nil {
		return ProcessIdentity{}, fmt.Errorf("read process %d pid namespace: %w", pid, err)
	}
	return ProcessIdentity{StartTime: start, Executable: filepath.Clean(exe), Namespace: procNS}, nil
}

// ProcessStart reads pid's birth marker, as ProcessIdentity.StartTime holds
// it, and whether the process has not exited. A zombie has exited. A pid
// that names no process is not alive, with a nil error.
func ProcessStart(pid int) (start string, alive bool, err error) {
	if pid <= 0 {
		return "", false, fmt.Errorf("process identity: %d is not a pid", pid)
	}
	state, start, err := readStat(pid)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return start, !exitedState(state), nil
}

// exitedState reports a /proc state letter of a process that has exited:
// a zombie, or one being reaped.
func exitedState(state string) bool {
	switch state {
	case "Z", "X", "x":
		return true
	}
	return false
}

func readStat(pid int) (state, start string, err error) {
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return "", "", fmt.Errorf("read process %d stat: %w", pid, err)
	}
	return parseStat(pid, stat)
}

// parseStat reads the state (field 3) and starttime (field 22, clock ticks
// since boot) of a /proc/<pid>/stat line. comm can contain spaces and
// parentheses. The final ')' is the end of that field, after which fields
// are whitespace-delimited, so starttime is token 20 of that suffix.
func parseStat(pid int, stat []byte) (state, start string, err error) {
	close := strings.LastIndexByte(string(stat), ')')
	if close < 0 || close+1 >= len(stat) {
		return "", "", fmt.Errorf("process %d stat has no comm terminator", pid)
	}
	fields := strings.Fields(string(stat[close+1:]))
	if len(fields) < 20 {
		return "", "", fmt.Errorf("process %d stat has %d fields, want start time", pid, len(fields))
	}
	return fields[0], fields[19], nil
}

// CurrentPIDNamespace returns the namespace marker for this process.
func CurrentPIDNamespace() string {
	ns, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return ""
	}
	return ns
}
