//go:build linux

package orphanreaper

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// On Linux a captured identity is the kernel's own values: starttime, field
// 22 of the leader's stat line exactly as the kernel prints it, and the
// boot id. A constant or a clock-derived time fails.
func TestIdentityIsTheKernelsStartAndBootID(t *testing.T) {
	held := holdProcess(t)
	pid := held.Process.Pid
	c, err := CaptureLeader(pid)
	if err != nil {
		t.Fatal(err)
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	// The held process is `sleep`, whose command name has no spaces.
	fields := strings.Fields(string(stat))
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}
	if want := (Identity{Boot: strings.TrimSpace(string(boot)), Start: fields[21]}); c.identity != want || want.Boot == "" {
		t.Fatalf("captured %+v, the kernel says %+v (%s)", c.identity, want, stat)
	}
}
