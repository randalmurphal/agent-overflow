//go:build darwin

package orphanreaper

import (
	"fmt"
	"testing"

	"golang.org/x/sys/unix"
)

// On macOS a captured identity is the kernel's P_starttime for the leader,
// read here straight from its process record. A constant or a clock-derived
// time fails.
func TestIdentityIsTheKernelsStartTime(t *testing.T) {
	held := holdProcess(t)
	pid := held.Process.Pid
	c, err := CaptureLeader(pid)
	if err != nil {
		t.Fatal(err)
	}
	proc, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		t.Fatal(err)
	}
	start := proc.Proc.P_starttime
	if want := (Identity{Start: fmt.Sprintf("%d.%06d", start.Sec, start.Usec)}); c.identity != want {
		t.Fatalf("captured %+v, the kernel says %+v", c.identity, want)
	}
}
