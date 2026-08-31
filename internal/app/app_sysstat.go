package app

import (
	"context"
	"log"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/sysstat"
)

// systemStatsInterval is how often the sampler emits a fresh
// system:stats event. 2s feels alive without burning cycles — the
// sidebar panel is glanceable, not a live profiler.
const systemStatsInterval = 2 * time.Second

// SystemStats is the wire shape pushed on the `system:stats` event
// channel. The sidebar SystemStatsFooter consumes it directly. Memory
// is in bytes; the frontend converts to GB for display so the
// precision stays on the producer side.
//
// system:stats is a latest-only replay channel (see
// transport/event_visibility.go): every frame is complete current
// state, so the bus retains and replays only the newest one.
//
// Not added to transport/event_visibility.go loopback-only set:
// coarse host CPU% + memory totals plus a single boolean OS-runtime
// hint (`isWsl`) are operational data a remote `--connect` operator
// would want to see, and disclose neither identity nor user content.
// Revisit if the channel ever grows per-process detail or anything
// tied to user identity.
type SystemStats struct {
	IsWSL         bool    `json:"isWsl"`
	CPUPercent    float64 `json:"cpuPercent"`
	MemUsedBytes  uint64  `json:"memUsedBytes"`
	MemTotalBytes uint64  `json:"memTotalBytes"`
}

// Test seams — production code never reassigns these. Lets app-level
// tests fake the sampler without spinning up real gopsutil reads.
var (
	systemStatsSampleFn = sysstat.Sample
	systemStatsPrimeFn  = sysstat.Prime
)

// startSystemStatsSampler runs the sidebar's CPU+memory sampler.
// Starts at app boot, exits when the lifeCtx is cancelled (Shutdown
// step 1b). Read-only — no FS writes, no subprocesses, no settings
// mutation. Mirrors the loop shape of `startRateLimitProbeLoop`.
//
// The first emit is deliberately at t = systemStatsInterval, not t=0:
// gopsutil computes CPU% as a delta against the prior `cpu.Percent`
// call, so emitting immediately after Prime would push a microsecond-
// window value (essentially noise rounding to 0). The frontend's
// `{#if stats}` guard hides the panel until the first real sample
// arrives, which reads cleaner than a 2s "0%" flash followed by the
// real number.
func (a *App) startSystemStatsSampler() {
	ctx := a.lifeCtx()
	go func() {
		// Prime seeds gopsutil's per-process delta state so the first
		// ticker emit has a meaningful window to compute against.
		systemStatsPrimeFn(ctx)

		ticker := time.NewTicker(systemStatsInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				a.emitSystemStats(ctx)
			}
		}
	}()
}

func (a *App) emitSystemStats(ctx context.Context) {
	if a.shuttingDown.Load() {
		return
	}
	reading, err := systemStatsSampleFn(ctx)
	if err != nil {
		// Sampler errors are rare in practice; gopsutil typically only
		// fails on exotic platforms or kernel/library mismatches. Log
		// at the standard level so a persistently-broken sampler is
		// diagnosable (matching the providerlifecycleapp probe pattern),
		// then skip the tick.
		log.Printf("sysstat: sample: %v", err)
		return
	}
	a.emit(eventchan.SystemStats, SystemStats{
		IsWSL:         wslIsWSL(),
		CPUPercent:    reading.CPUPercent,
		MemUsedBytes:  reading.MemUsedBytes,
		MemTotalBytes: reading.MemTotalBytes,
	})
}
