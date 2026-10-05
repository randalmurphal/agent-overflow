//go:build !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"agent-overflow/internal/harness/containment"
	"agent-overflow/internal/harness/instanceinfo"
	"agent-overflow/internal/harnessclient"
	"agent-overflow/internal/procutil"
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
	if err := procutil.SignalGroup(command.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("kill owned process group: %w", err)
	}
	return nil
}

// terminateProcessTreeVerified stops the launched tree. Its root is a launcher
// chain (pnpm is a `#!/usr/bin/env node` script), so the root's executable
// changes after Start while its kernel start time and PID namespace do not.
// Each attempt verifies the root under its current executable, and an exec
// during an attempt starts another one against the new executable.
func terminateProcessTreeVerified(command *exec.Cmd, _ containment.Group, launched instanceinfo.ProcessIdentity) error {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	pid := command.Process.Pid
	for {
		root := launchedRootIdentity(pid, launched)
		err := harnessclient.TerminateProcessTreeVerified(ctx, pid, root, 2*time.Second)
		if err == nil || ctx.Err() != nil || launchedRootIdentity(pid, launched) == root {
			return err
		}
	}
}

// launchedRootIdentity returns pid's current identity while it is still the
// launched root: same start time and PID namespace, whatever it has exec'd
// since. Otherwise it returns the launch record, which the verified teardown
// treats as gone or refuses as a recycled PID.
func launchedRootIdentity(pid int, launched instanceinfo.ProcessIdentity) instanceinfo.ProcessIdentity {
	current, err := instanceinfo.CaptureProcessIdentity(pid)
	if err != nil || current.StartTime != launched.StartTime || current.Namespace != launched.Namespace {
		return launched
	}
	return current
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
	return !errors.Is(procutil.SignalGroup(pid, 0), os.ErrProcessDone)
}
