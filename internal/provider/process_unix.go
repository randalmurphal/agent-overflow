//go:build !windows

package provider

import "syscall"

// signalGroupPlatform sends sig to the process group identified by pid.
// The negative-PID convention here is documented in kill(2): -pgid
// targets every process whose pgid matches the absolute value. This is
// valid on every Unix target, so it stays in the shared !windows file;
// applySysProcAttr is split further by GOOS because Pdeathsig is
// Linux-only (see process_linux.go / process_darwin.go).
func signalGroupPlatform(pid int, sig syscall.Signal) {
	_ = syscall.Kill(-pid, sig)
}
