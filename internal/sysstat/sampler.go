package sysstat

import (
	"context"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/mem"
)

// Reading is one snapshot of host CPU + memory usage.
type Reading struct {
	// CPUPercent is the total CPU utilisation across cores, 0–100,
	// computed as the delta since gopsutil's previous cpu.Percent
	// call. The very first call after process start returns 0 — call
	// Prime once before entering a sampling loop so the first
	// user-visible Sample has a meaningful delta.
	CPUPercent float64
	// MemUsedBytes follows the gopsutil convention: on Linux this is
	// total - free - buffers - cached (matches htop). On macOS it
	// comes from the Mach host_statistics path.
	MemUsedBytes  uint64
	MemTotalBytes uint64
}

// Indirection points so tests can substitute the gopsutil calls.
// Production code never reassigns these.
var (
	readCPUPercent = func(ctx context.Context) ([]float64, error) {
		// interval=0, percpu=false → one aggregated value, delta since
		// the previous call (per gopsutil docs).
		return cpu.PercentWithContext(ctx, 0, false)
	}
	readMem = func(ctx context.Context) (*mem.VirtualMemoryStat, error) {
		return mem.VirtualMemoryWithContext(ctx)
	}
)

// Prime performs a throwaway CPU read to seed gopsutil's per-process
// delta state. Cheap (one /proc/stat read on Linux, one Mach call on
// macOS). Safe to call multiple times.
func Prime(ctx context.Context) {
	_, _ = readCPUPercent(ctx)
}

// Sample returns a fresh host reading. Errors propagate from gopsutil
// — they're rare in practice (typically only on exotic platforms or
// kernel/library mismatches), and the caller is expected to skip the
// affected tick rather than retry tightly.
func Sample(ctx context.Context) (Reading, error) {
	percents, err := readCPUPercent(ctx)
	if err != nil {
		return Reading{}, err
	}
	vm, err := readMem(ctx)
	if err != nil {
		return Reading{}, err
	}
	return Reading{
		CPUPercent:    firstOrZero(percents),
		MemUsedBytes:  vm.Used,
		MemTotalBytes: vm.Total,
	}, nil
}

// firstOrZero coerces gopsutil's []float64 return to a single value.
// Empty slice is the wire-confirmed "no data this tick" shape and must
// not panic — it folds to 0 so the UI just shows a momentary 0% rather
// than crashing the goroutine.
func firstOrZero(percents []float64) float64 {
	if len(percents) == 0 {
		return 0
	}
	return percents[0]
}
