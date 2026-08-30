//go:build !windows

package main

import (
	"os"
	"syscall"
)

func terminateSelf() error {
	return syscall.Kill(os.Getpid(), syscall.SIGTERM)
}
