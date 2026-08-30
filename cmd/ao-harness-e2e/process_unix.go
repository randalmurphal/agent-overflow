//go:build !windows

package main

import (
	"context"
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"agent-overflow/internal/harness/containment"
	"agent-overflow/internal/harness/instanceinfo"
	"agent-overflow/internal/harnessclient"
)

func configureProcessGroup(command *exec.Cmd) {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.Setpgid = true
}

func terminateProcessTree(command *exec.Cmd, _ containment.Group) error {
	if command.Process == nil {
		return nil
	}
	if err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return fmt.Errorf("kill owned process group: %w", err)
	}
	return nil
}

func terminateProcessTreeVerified(command *exec.Cmd, group containment.Group, identity instanceinfo.ProcessIdentity) error {
	_ = group
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	return harnessclient.TerminateProcessTreeVerified(ctx, command.Process.Pid, identity, 2*time.Second)
}

func waitForProcessTree(command *exec.Cmd, _ containment.Group, timeout time.Duration) bool {
	if command.Process == nil {
		return true
	}
	deadline := time.Now().Add(timeout)
	for {
		if !processGroupAlive(command.Process.Pid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func processGroupAlive(pid int) bool {
	err := syscall.Kill(-pid, 0)
	return err == nil || err == syscall.EPERM
}
