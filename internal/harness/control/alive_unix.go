//go:build !windows

package control

import (
	"errors"
	"os"
	"syscall"
)

// processAlive reports whether pid names a live process, using signal 0
// — the standard "check, don't deliver" probe. EPERM counts as alive:
// the process exists, it just belongs to another user, which is a fact
// about ownership rather than about liveness.
//
// Deliberately a local copy of instanceinfo.ProcessAlive rather than an
// import: this package is linked into ao-mockprovider, whose whole point
// is to be a small standalone binary, and instanceinfo carries the
// instance registry (cache-dir discovery, JSON rows) that a mock has no
// business knowing about.
//
// Racy by nature — a pid can die between the probe and the caller's next
// line — and that is fine: the registry uses it to mark a registration
// dead, and a mock that dies a millisecond later is still dead.
func processAlive(pid int) bool {
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
