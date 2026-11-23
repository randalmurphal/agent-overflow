package orphanreaper

import (
	"errors"
	"fmt"

	"agent-overflow/internal/harness/instanceinfo"
)

// Identity tells the process running as a pid from any process that had or
// later gets the same pid, whichever process reads it and whatever the wall
// clock says. Start is the birth marker the kernel stores for the process
// (instanceinfo.ProcessStart): starttime in clock ticks since boot on Linux,
// P_starttime on macOS. Boot is the kernel's boot id on Linux, where
// starttime restarts at every boot; it is empty on macOS, whose start time
// is absolute. Both are compared, never interpreted: two reads of one pid
// name the same process exactly when their Identities are ==.
type Identity struct {
	Boot  string `json:"boot,omitempty"`
	Start string `json:"start"`
}

// ErrNoProcess is a pid that names no process.
var ErrNoProcess = errors.New("no such process")

// Identify reads the identity of the process running as pid. The recording
// side and the sweep both read it here. An exited process that is not yet
// reaped keeps its identity. An error wraps ErrNoProcess when pid names no
// process.
func Identify(pid int) (Identity, error) {
	start, _, err := instanceinfo.ProcessStart(pid)
	if err != nil {
		return Identity{}, fmt.Errorf("read process %d start: %w", pid, err)
	}
	if start == "" {
		return Identity{}, fmt.Errorf("process %d: %w", pid, ErrNoProcess)
	}
	boot, err := bootID()
	if err != nil {
		return Identity{}, err
	}
	return Identity{Boot: boot, Start: start}, nil
}
