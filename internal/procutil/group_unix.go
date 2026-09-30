//go:build !windows

package procutil

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// ConfigureGroup starts the command in its own session, which leads a new
// process group whose id is its pid, and makes context cancellation kill that
// whole group. A setup hook or a check command routinely spawns children
// (`sh -c 'make … & wait'`); killing only the direct child leaves them holding
// the worktree open past the timeout that was supposed to end them.
//
// A new session rather than only a new group, because a group in this
// process's session is a background group of its controlling terminal, if it
// has one: a descendant that reads the terminal, or an interactive bash or zsh
// taking job control, stops the whole group with SIGTTIN. Without a
// controlling terminal, /dev/tty fails instead.
//
// WaitDelay bounds how long Wait blocks on inherited pipes after the kill, so a
// grandchild that ignored SIGKILL delivery ordering cannot wedge the reaper.
func ConfigureGroup(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	command.Cancel = func() error { return signalConfiguredGroup(command, syscall.SIGKILL) }
	command.WaitDelay = time.Second
}

// KillConfiguredGroup applies the same process-group boundary used by
// context cancellation. Callers that own a command must use this instead of
// Process.Kill, which leaves descendants behind.
func KillConfiguredGroup(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return os.ErrProcessDone
	}
	if !groupConfigured(command) {
		return errors.New("process group was not configured")
	}
	return signalConfiguredGroup(command, syscall.SIGKILL)
}

// TerminateConfiguredGroup asks every member of the configured group to exit
// with SIGTERM. Callers pair it with KillConfiguredGroup after a grace period.
func TerminateConfiguredGroup(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return os.ErrProcessDone
	}
	if !groupConfigured(command) {
		return errors.New("process group was not configured")
	}
	return signalConfiguredGroup(command, syscall.SIGTERM)
}

// ConfiguredGroupAlive reports whether any process still belongs to the
// command's group. It is meaningful after Wait returned: the leader is reaped,
// so a live group means descendants outlived the command.
func ConfiguredGroupAlive(command *exec.Cmd) bool {
	if command == nil || command.Process == nil {
		return false
	}
	err := syscall.Kill(-command.Process.Pid, 0)
	return !errors.Is(err, syscall.ESRCH)
}

// groupConfigured reports whether the command leads its own process group.
func groupConfigured(command *exec.Cmd) bool {
	attr := command.SysProcAttr
	return attr != nil && (attr.Setsid || attr.Setpgid)
}

func signalConfiguredGroup(command *exec.Cmd, signal syscall.Signal) error {
	if command.Process == nil {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-command.Process.Pid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
