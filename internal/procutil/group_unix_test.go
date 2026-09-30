//go:build !windows

package procutil

import (
	"context"
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"
)

// A configured command leads its own session, so it has no controlling
// terminal whose job control could stop its group, and its group id is its
// pid, which the group kill targets.
func TestConfigureGroupStartsANewSession(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "sleep", "30")
	ConfigureGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		_ = KillConfiguredGroup(cmd)
		_ = cmd.Wait()
	}()
	pid := cmd.Process.Pid
	sid, err := unix.Getsid(pid)
	if err != nil {
		t.Fatalf("getsid: %v", err)
	}
	pgid, err := unix.Getpgid(pid)
	if err != nil {
		t.Fatalf("getpgid: %v", err)
	}
	if sid != pid || pgid != pid {
		t.Fatalf("session %d, group %d; want both %d", sid, pgid, pid)
	}
	if !ConfiguredGroupAlive(cmd) {
		t.Fatal("the configured group is not alive")
	}
	if err := KillConfiguredGroup(cmd); err != nil {
		t.Fatalf("kill the configured group: %v", err)
	}
}
