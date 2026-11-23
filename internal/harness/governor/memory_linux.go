//go:build linux

package governor

import (
	"fmt"

	"agent-overflow/internal/hostmemory"
)

// linuxMemory reads the host at /proc and the cgroup file system mounted at
// cgroup (hostmemory.ReadAt).
type linuxMemory struct{ proc, cgroup string }

// AvailableMemory is what this process can still allocate: MemAvailable, or
// its cgroups' headroom when that is lower, as on a large host whose run is
// confined to a small cgroup.
func (m linuxMemory) AvailableMemory() (uint64, error) {
	r, err := hostmemory.ReadAt(m.proc, m.cgroup)
	if err != nil {
		return 0, fmt.Errorf("harness governor: %w", err)
	}
	return r.Headroom(), nil
}

func defaultMemory() MemoryReader { return linuxMemory{proc: "/proc", cgroup: "/sys/fs/cgroup"} }
