//go:build linux

package instanceinfo

import (
	"fmt"
	"os"
	"path/filepath"
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
	stat, err := os.ReadFile(filepath.Join(base, "stat"))
	if err != nil {
		return ProcessIdentity{}, fmt.Errorf("read process %d stat: %w", pid, err)
	}
	// comm can contain spaces and parentheses. The final ')' is the end of
	// that field, after which fields are whitespace-delimited. starttime is
	// field 22, or token 20 in the suffix beginning at field 3.
	close := strings.LastIndexByte(string(stat), ')')
	if close < 0 || close+1 >= len(stat) {
		return ProcessIdentity{}, fmt.Errorf("process %d stat has no comm terminator", pid)
	}
	fields := strings.Fields(string(stat[close+1:]))
	if len(fields) < 20 {
		return ProcessIdentity{}, fmt.Errorf("process %d stat has %d fields, want start time", pid, len(fields))
	}
	exe, err := os.Readlink(filepath.Join(base, "exe"))
	if err != nil {
		return ProcessIdentity{}, fmt.Errorf("read process %d executable: %w", pid, err)
	}
	procNS, err := os.Readlink(filepath.Join(base, "ns", "pid"))
	if err != nil {
		return ProcessIdentity{}, fmt.Errorf("read process %d pid namespace: %w", pid, err)
	}
	return ProcessIdentity{StartTime: fields[19], Executable: filepath.Clean(exe), Namespace: procNS}, nil
}

// CurrentPIDNamespace returns the namespace marker for this process.
func CurrentPIDNamespace() string {
	ns, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return ""
	}
	return ns
}
