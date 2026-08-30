//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"agent-overflow/internal/harness/governor"
	"agent-overflow/internal/harness/instanceinfo"
)

func launchDetachedWatchdog(dataRoot, stderrPath string, lease governor.Lease) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	root, err := instanceinfo.CanonicalPath(dataRoot)
	if err != nil {
		_ = logFile.Close()
		return fmt.Errorf("canonicalize watchdog root: %w", err)
	}
	readyPath := watchdogReadyPath(root)
	if err := removeWatchdogReadyFile(readyPath); err != nil {
		_ = logFile.Close()
		return err
	}
	cmd := exec.Command(self, "--watchdog", "--data-dir", root, "--lease-id", lease.ID, "--owner-pid", fmt.Sprint(lease.OwnerPID), "--owner-birth-id", lease.OwnerBirthID, "--ready-file", readyPath)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x00000008}
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("start detached watchdog: %w", err)
	}
	if err := logFile.Close(); err != nil {
		return err
	}
	return awaitDetachedWatchdogReady(cmd.Process, root, lease, readyPath)
}
