//go:build darwin

package procutil

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// startGroup starts script in a configured group and returns once no
// member is running but sleepers, leaving the leader unreaped.
func startGroup(t *testing.T, script string, running int) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "sh", "-c", script)
	ConfigureGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		members, err := RunningGroupMembers(cmd.Process.Pid)
		if err != nil {
			t.Fatalf("list the group: %v", err)
		}
		if len(members) == running && !RunningInGroup(cmd.Process.Pid, cmd.Process.Pid) {
			return cmd
		}
		if time.Now().After(deadline) {
			t.Fatalf("group %d still runs %v, want %d members", cmd.Process.Pid, members, running)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// macOS refuses a signal to a group whose members have all exited but wait
// to be reaped with EPERM. That group is done: the kill a caller makes
// before reaping must not fail, and the group is not alive.
func TestSignalGroupTreatsAnExitedUnreapedGroupAsDone(t *testing.T) {
	cmd := startGroup(t, "exit 3", 0)
	pgid := cmd.Process.Pid
	if err := syscall.Kill(-pgid, 0); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("kill(-%d, 0) = %v; the platform no longer refuses an exited group with EPERM", pgid, err)
	}
	if err := KillConfiguredGroup(cmd); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("KillConfiguredGroup = %v, want os.ErrProcessDone", err)
	}
	if err := TerminateConfiguredGroup(cmd); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("TerminateConfiguredGroup = %v, want os.ErrProcessDone", err)
	}
	if ConfiguredGroupAlive(cmd) {
		t.Fatal("a group whose members have all exited is reported alive")
	}
	var exitErr *exec.ExitError
	if err := cmd.Wait(); !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
		t.Fatalf("wait = %v, want exit status 3", err)
	}
}

// A member still running keeps the group alive after its leader exited,
// and the kill reaches it.
func TestSignalGroupReachesAMemberThatOutlivedTheLeader(t *testing.T) {
	cmd := startGroup(t, "sleep 30 & exit 0", 1)
	if !ConfiguredGroupAlive(cmd) {
		t.Fatal("a group with a running member is reported gone")
	}
	members, err := RunningGroupMembers(cmd.Process.Pid)
	if err != nil || len(members) != 1 {
		t.Fatalf("members %v (%v), want the sleeper", members, err)
	}
	if err := KillConfiguredGroup(cmd); err != nil {
		t.Fatalf("KillConfiguredGroup = %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for RunningInGroup(members[0], cmd.Process.Pid) {
		if time.Now().After(deadline) {
			t.Fatalf("member %d outlived the group kill", members[0])
		}
		time.Sleep(5 * time.Millisecond)
	}
}
