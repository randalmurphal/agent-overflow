//go:build windows

package orphanreaper

import (
	"errors"
	"syscall"
)

// killGroup refuses on Windows: the desktop binary never spawns provider
// children there (the WSL-side Linux backend owns them, and the launcher
// uses a Win32 Job Object for lifetime management). It exists so the
// package compiles for Windows.
func killGroup(pgid int, sig syscall.Signal) error {
	_ = pgid
	_ = sig
	return errors.New("process groups are not signalled on windows")
}
