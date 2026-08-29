//go:build windows

package main

import (
	"fmt"
	"os/exec"
	"time"

	"agent-overflow/internal/harness/containment"
	"agent-overflow/internal/harness/instanceinfo"
)

func configureProcessGroup(*exec.Cmd) {}

func terminateProcessTree(_ *exec.Cmd, group containment.Group) error {
	killer, ok := group.(containment.Killer)
	if !ok {
		return fmt.Errorf("memory boundary cannot terminate its owned process tree")
	}
	return killer.Kill()
}

func terminateProcessTreeVerified(command *exec.Cmd, group containment.Group, identity instanceinfo.ProcessIdentity) error {
	// The Job Object is the exact ownership proof on Windows. It remains safe
	// to terminate after the root exits, when PID identity can no longer be
	// observed, because no external PID is addressed.
	_ = identity
	return terminateProcessTree(command, group)
}

func waitForProcessTree(command *exec.Cmd, group containment.Group, timeout time.Duration) bool {
	if waiter, ok := group.(containment.Waiter); ok {
		return waiter.Wait(timeout) == nil
	}
	_ = command
	_ = timeout
	return false
}
