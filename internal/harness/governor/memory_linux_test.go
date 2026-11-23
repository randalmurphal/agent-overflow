//go:build linux

package governor

import (
	"os"
	"path/filepath"
	"testing"
)

// TestACgroupLimitBelowTheHostsHeadroomCapsTheGrant reserves 1 GiB units on
// a host with 64 GiB available, from a cgroup with 3 GiB of room left: the
// grant is what the cgroup has room for.
func TestACgroupLimitBelowTheHostsHeadroomCapsTheGrant(t *testing.T) {
	const gib = 1 << 30
	root := t.TempDir()
	for rel, value := range map[string]string{
		"proc/meminfo":              "MemTotal: 134217728 kB\nMemAvailable: 67108864 kB\n",
		"proc/self/cgroup":          "0::/job\n",
		"cgroup/job/memory.max":     "4294967296\n",
		"cgroup/job/memory.current": "1073741824\n",
		"cgroup/job/memory.stat":    "inactive_file 0\n",
	} {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m, err := New(Options{Dir: t.TempDir(), AvailableFloorBytes: 1, Memory: linuxMemory{proc: filepath.Join(root, "proc"), cgroup: filepath.Join(root, "cgroup")}})
	if err != nil {
		t.Fatal(err)
	}
	units := []uint64{gib, gib, gib, gib, gib, gib, gib, gib}
	lease, granted, err := m.ReserveUpTo(Request{RunID: "run", Worktree: t.TempDir(), DataRoot: t.TempDir(), OwnerPID: os.Getpid()}, units)
	if err != nil || granted != 2 || lease.CeilingBytes != 2*gib {
		t.Fatalf("granted %d (%d bytes), %v; want the 2 units below the cgroup's 3 GiB of room less the floor", granted, lease.CeilingBytes, err)
	}
}
