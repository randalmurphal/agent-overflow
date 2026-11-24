//go:build windows

package main

import "syscall"

// watchdogSysProcAttr starts the watchdog as a detached process
// (DETACHED_PROCESS) with no window, so it outlives the console of the
// command that launched it.
func watchdogSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x00000008}
}
