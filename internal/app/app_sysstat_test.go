package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"agent-overflow/internal/sysstat"
)

// TestEmitSystemStats_HappyPath confirms a successful sample lands on
// the `system:stats` channel with the sampler's reading copied across.
func TestEmitSystemStats_HappyPath(t *testing.T) {
	app := newTestAppWithStore(t)

	origSample := systemStatsSampleFn
	t.Cleanup(func() { systemStatsSampleFn = origSample })
	systemStatsSampleFn = func(context.Context) (sysstat.Reading, error) {
		return sysstat.Reading{
			CPUPercent:    42.5,
			MemUsedBytes:  4 << 30,
			MemTotalBytes: 16 << 30,
		}, nil
	}

	got := make(chan SystemStats, 1)
	app.testEmitHook = func(name string, data any) {
		if name != "system:stats" {
			return
		}
		s, ok := data.(SystemStats)
		if !ok {
			t.Errorf("system:stats payload type = %T, want SystemStats", data)
			return
		}
		select {
		case got <- s:
		default:
		}
	}

	app.emitSystemStats(context.Background())

	select {
	case s := <-got:
		if s.CPUPercent != 42.5 || s.MemUsedBytes != 4<<30 || s.MemTotalBytes != 16<<30 {
			t.Fatalf("payload = %+v, want fields copied from sampler reading", s)
		}
	case <-time.After(time.Second):
		t.Fatal("no system:stats event emitted")
	}
}

// TestEmitSystemStats_SkipsOnShutdown pins the shuttingDown guard. A
// regression that drops the guard would push stats after Shutdown
// begins, racing the transport teardown.
func TestEmitSystemStats_SkipsOnShutdown(t *testing.T) {
	app := newTestAppWithStore(t)
	app.shuttingDown.Store(true)

	origSample := systemStatsSampleFn
	t.Cleanup(func() { systemStatsSampleFn = origSample })
	sampleCalled := false
	systemStatsSampleFn = func(context.Context) (sysstat.Reading, error) {
		sampleCalled = true
		return sysstat.Reading{}, nil
	}

	emitted := false
	app.testEmitHook = func(string, any) { emitted = true }

	app.emitSystemStats(context.Background())

	if sampleCalled {
		t.Error("Sample should not run after shuttingDown is set")
	}
	if emitted {
		t.Error("emit should not fire after shuttingDown is set")
	}
}

// TestEmitSystemStats_SkipsOnSampleError pins the error-skip branch.
// gopsutil errors are rare but recoverable; emitting a zero-valued
// payload would push a misleading "0% / 0 GB" reading to the sidebar.
func TestEmitSystemStats_SkipsOnSampleError(t *testing.T) {
	app := newTestAppWithStore(t)

	origSample := systemStatsSampleFn
	t.Cleanup(func() { systemStatsSampleFn = origSample })
	systemStatsSampleFn = func(context.Context) (sysstat.Reading, error) {
		return sysstat.Reading{}, errors.New("gopsutil boom")
	}

	emitted := false
	app.testEmitHook = func(string, any) { emitted = true }

	app.emitSystemStats(context.Background())

	if emitted {
		t.Error("emit should be skipped on sampler error")
	}
}

// TestStartSystemStatsSampler_ExitsOnAppCtxCancel pins Step 1b's
// wiring: a regression that swapped a.lifeCtx() back to
// context.Background() (or dropped the <-ctx.Done() select arm) would
// keep the goroutine alive after Shutdown returned. Mirrors
// TestStartRateLimitProbeLoop_ExitsOnAppCtxCancel.
func TestStartSystemStatsSampler_ExitsOnAppCtxCancel(t *testing.T) {
	app := newTestAppWithStore(t)

	origSample := systemStatsSampleFn
	origPrime := systemStatsPrimeFn
	t.Cleanup(func() {
		systemStatsSampleFn = origSample
		systemStatsPrimeFn = origPrime
	})

	primed := make(chan struct{}, 1)
	systemStatsPrimeFn = func(context.Context) {
		select {
		case primed <- struct{}{}:
		default:
		}
	}
	sampled := make(chan struct{}, 4)
	systemStatsSampleFn = func(context.Context) (sysstat.Reading, error) {
		select {
		case sampled <- struct{}{}:
		default:
		}
		return sysstat.Reading{}, nil
	}

	app.startSystemStatsSampler()

	// Confirm Prime ran so the goroutine reached its select-loop body.
	select {
	case <-primed:
	case <-time.After(time.Second):
		t.Fatal("Prime never fired")
	}

	app.appCancel()

	// systemStatsInterval is 2s in production, so any sample inside
	// this window after appCancel is a spurious post-cancel call.
	select {
	case <-sampled:
		t.Fatal("Sample fired after appCancel — loop did not honour ctx.Done()")
	case <-time.After(100 * time.Millisecond):
	}
}
