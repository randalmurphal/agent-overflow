//go:build !windows

package orphanreaper

import (
	"errors"
	"os"
	"os/exec"
	"testing"

	"github.com/shirou/gopsutil/v4/process"
)

// holdProcess starts a sleep this test ends.
func holdProcess(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "600")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

// A capture names the leader by the identity a later read of the same pid
// returns, records this process as its parent and carries the start time an
// earlier version reads; a pid that no longer names a process cannot be
// captured.
func TestIdentityOfACapturedLeaderUntilItIsGone(t *testing.T) {
	held := holdProcess(t)
	pid := held.Process.Pid
	c, err := CaptureLeader(pid)
	if err != nil {
		t.Fatal(err)
	}
	again, err := observe(SystemProcInfo(), pid)
	if err != nil || again.Identity != c.identity || again.PPID != os.Getpid() {
		t.Fatalf("a later read = %+v %v, want identity %+v under parent %d", again, err, c.identity, os.Getpid())
	}
	if c.leader != pid || c.parent != os.Getpid() || c.identity.Start == "" {
		t.Fatalf("capture %+v, want leader %d under parent %d", c, pid, os.Getpid())
	}
	// The earlier format's start time is gopsutil's, which in a container
	// can read a whole second apart between two reads.
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		t.Fatal(err)
	}
	now, err := p.CreateTime()
	if err != nil {
		t.Fatal(err)
	}
	if diff := c.createUnix - now; c.createUnix <= 0 || diff < -1000 || diff > 1000 {
		t.Fatalf("capture carries create_unix %d, gopsutil now reads %d", c.createUnix, now)
	}
	if err := held.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = held.Wait()
	if c, err := CaptureLeader(pid); !errors.Is(err, ErrNoProcess) {
		t.Fatalf("a reaped process captures %+v %v, want ErrNoProcess", c, err)
	}
	if _, err := CaptureLeader(1); err == nil {
		t.Fatal("init was captured as a group leader")
	}
}
