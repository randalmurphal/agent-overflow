package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"agent-overflow/internal/provider/claude"
)

// usageProbeMinInterval is the floor between two automatic usage probes for
// one provider. Turn completion is the chattiest trigger — modern Claude CLIs
// emit several EventTurnComplete per logical turn (soft round closes at
// intermediate message boundaries), and ungated each one issued its own
// request against the usage endpoint; bursts like that are what earn a 429.
const usageProbeMinInterval = 30 * time.Second

// defaultUsageProbeBackoff applies after a 429 whose Retry-After header was
// absent or unusable.
const defaultUsageProbeBackoff = time.Minute

// usageProbeGate bounds one provider's usage-probe traffic. Concurrent
// triggers collapse into the in-flight probe plus at most one trailing run;
// triggers inside the cooldown collapse into that same trailing run; a
// server-imposed backoff (429) holds automatic probes until it expires. The
// trailing run exists because the last trigger of a burst is the one carrying
// the freshest turn cost — dropping it outright would leave the rings stale
// until the next periodic tick.
//
// The gate recognizes claude.RateLimitedError as the backoff signal; the
// Codex probe never produces one, so its gate only ever coalesces.
type usageProbeGate struct {
	probe func(context.Context) error
	ctxFn func() context.Context

	// now and afterFunc are test seams; production uses the wall clock.
	now       func() time.Time
	afterFunc func(time.Duration, func())

	mu           sync.Mutex
	lastStart    time.Time
	inFlight     bool
	trailing     bool
	timerSet     bool
	backoffUntil time.Time
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
// trailing run when a probe is in flight, inside the cooldown, or inside a
// backoff. Runs the probe synchronously on the caller's goroutine when it
// runs at all.
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

	err := g.probe(g.ctxFn())

	g.mu.Lock()
	g.inFlight = false
	g.noteResultLocked(err)
	if g.trailing {
		g.trailing = false
		g.scheduleLocked(g.holdLocked(g.now()))
	}
	g.mu.Unlock()
}

// NoteResult records the outcome of a probe the gate did not run itself (the
// manual refresh path), so a 429 from any path holds automatic probes too.
func (g *usageProbeGate) NoteResult(err error) {
	g.mu.Lock()
	g.noteResultLocked(err)
	g.mu.Unlock()
}

// BackoffRemaining reports how much of a server-imposed backoff still holds.
// Zero means requests are allowed.
func (g *usageProbeGate) BackoffRemaining() time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	if remaining := g.backoffUntil.Sub(g.now()); remaining > 0 {
		return remaining
	}
	return 0
}

// holdLocked returns how long automatic probing must still wait: the longer
// of the cooldown remainder and any server-imposed backoff. Non-positive
// means clear to run.
func (g *usageProbeGate) holdLocked(now time.Time) time.Duration {
	wait := usageProbeMinInterval - now.Sub(g.lastStart)
	if backoff := g.backoffUntil.Sub(now); backoff > wait {
		wait = backoff
	}
	return wait
}

// scheduleLocked arms the single trailing timer. An already-armed timer
// absorbs the trigger: its firing re-enters request(), which re-derives the
// wait from current state, so a backoff extended meanwhile is still honored.
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

func (g *usageProbeGate) noteResultLocked(err error) {
	var limited *claude.RateLimitedError
	if !errors.As(err, &limited) {
		return
	}
	retry := limited.RetryAfter
	if retry <= 0 {
		retry = defaultUsageProbeBackoff
	}
	g.backoffUntil = g.now().Add(retry)
}

// claudeUsageGate lazily builds the Claude gate so directly-constructed test
// Apps get one without boot wiring.
func (a *App) claudeUsageGate() *usageProbeGate {
	a.claudeUsageGateOnce.Do(func() {
		a.claudeUsageGateVal = newUsageProbeGate(a.probeClaudeRateLimits, a.lifeCtx)
	})
	return a.claudeUsageGateVal
}

func (a *App) codexUsageGate() *usageProbeGate {
	a.codexUsageGateOnce.Do(func() {
		a.codexUsageGateVal = newUsageProbeGate(a.probeCodexRateLimits, a.lifeCtx)
	})
	return a.codexUsageGateVal
}
