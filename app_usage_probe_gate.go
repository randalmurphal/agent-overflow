package main

import (
	"context"
	"sync"
	"time"
)

// usageProbeMinInterval is the floor between two automatic usage probes for
// one provider. The automatic triggers left are sparse — the startup account
// probe and the activity-gated 2-minute poll loop (turn completion only marks
// activity; see startRateLimitProbeLoop) — so this floor is a backstop
// against those overlapping, not a rate limiter in its own right.
const usageProbeMinInterval = 30 * time.Second

// usageProbeGate bounds one provider's automatic usage-probe traffic.
// Concurrent triggers collapse into the in-flight probe plus at most one
// trailing run; triggers inside the cooldown collapse into that same trailing
// run. The trailing run exists because the last trigger of a burst is the one
// carrying the freshest turn cost — dropping it outright would leave the rings
// stale until the next periodic tick.
//
// Server-imposed 429 backoffs are deliberately NOT tracked here: the throttle
// is per-account, not per-provider, so that state lives in usagebackoff.Ledger
// (internal/usagebackoff), which the refresh path consults before touching the
// endpoint. A probe the gate lets through can therefore still return without
// having sent a request.
type usageProbeGate struct {
	probe func(context.Context) error
	ctxFn func() context.Context

	// now and afterFunc are test seams; production uses the wall clock.
	now       func() time.Time
	afterFunc func(time.Duration, func())

	mu        sync.Mutex
	lastStart time.Time
	inFlight  bool
	trailing  bool
	timerSet  bool
}

func newUsageProbeGate(
	probe func(context.Context) error,
	ctxFn func() context.Context,
) *usageProbeGate {
	return &usageProbeGate{
		probe: probe,
		ctxFn: ctxFn,
		now:   time.Now,
		afterFunc: func(d time.Duration, f func()) {
			time.AfterFunc(d, f)
		},
	}
}

// Request runs the probe now, or folds this trigger into the single pending
// trailing run when a probe is in flight or inside the cooldown. Runs the
// probe synchronously on the caller's goroutine when it runs at all.
func (g *usageProbeGate) Request() { g.request(false) }

func (g *usageProbeGate) request(fromTimer bool) {
	g.mu.Lock()
	if fromTimer {
		g.timerSet = false
	}
	if g.inFlight {
		g.trailing = true
		g.mu.Unlock()
		return
	}
	if wait := g.holdLocked(g.now()); wait > 0 {
		g.scheduleLocked(wait)
		g.mu.Unlock()
		return
	}
	g.inFlight = true
	g.lastStart = g.now()
	g.mu.Unlock()

	// The probe's error surface is owned elsewhere: probeClaudeRateLimits /
	// probeCodexRateLimits log their own failures and usagebackoff.Ledger
	// records any 429.
	_ = g.probe(g.ctxFn())

	g.mu.Lock()
	g.inFlight = false
	if g.trailing {
		g.trailing = false
		g.scheduleLocked(g.holdLocked(g.now()))
	}
	g.mu.Unlock()
}

// holdLocked returns how much of the cooldown automatic probing must still
// wait out. Non-positive means clear to run.
func (g *usageProbeGate) holdLocked(now time.Time) time.Duration {
	return usageProbeMinInterval - now.Sub(g.lastStart)
}

// scheduleLocked arms the single trailing timer. An already-armed timer
// absorbs the trigger: its firing re-enters request(), which re-derives the
// wait from current state.
func (g *usageProbeGate) scheduleLocked(wait time.Duration) {
	if g.timerSet {
		return
	}
	g.timerSet = true
	if wait < 0 {
		wait = 0
	}
	g.afterFunc(wait, func() { g.request(true) })
}

// claudeUsageGate lazily builds the Claude gate so directly-constructed test
// Apps get one without boot wiring.
func (a *App) claudeUsageGate() *usageProbeGate {
	a.usageProbe.claudeGateOnce.Do(func() {
		a.usageProbe.claudeGate = newUsageProbeGate(a.probeClaudeRateLimits, a.lifeCtx)
	})
	return a.usageProbe.claudeGate
}

func (a *App) codexUsageGate() *usageProbeGate {
	a.usageProbe.codexGateOnce.Do(func() {
		a.usageProbe.codexGate = newUsageProbeGate(a.probeCodexRateLimits, a.lifeCtx)
	})
	return a.usageProbe.codexGate
}
