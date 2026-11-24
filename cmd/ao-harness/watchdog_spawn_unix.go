//go:build !windows

package main

import "syscall"

// watchdogSysProcAttr starts the watchdog in its own session, so it outlives
// the terminal and process group of the command that launched it.
func watchdogSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
