//go:build !windows

package instanceinfo

import (
	"errors"
	"os"
	"syscall"
)

// ProcessAlive reports whether pid names a live process, using signal 0
// — the standard "check, don't deliver" probe. EPERM counts as alive:
// the process exists, it just belongs to another user, which is a fact
// about ownership rather than about liveness.
//
// Racy by nature (a pid can die between the probe and the caller's next
// line) and that is fine: the registry uses it to mark stale rows, and
// a row that goes stale a millisecond later is still stale.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
