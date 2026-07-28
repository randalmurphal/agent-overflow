package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"agent-overflow/internal/provider/claude"
)

// gateClock is a deterministic clock plus timer capture for usageProbeGate.
// The gate runs its probe synchronously on the requesting goroutine, so a
// single-goroutine test fully controls interleaving: advance the clock, fire
// the captured timer callbacks by hand.
type gateClock struct {
	now    time.Time
	timers []gateTimer
}

type gateTimer struct {
	wait time.Duration
	fn   func()
}

func newGateClock() *gateClock {
	return &gateClock{now: time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)}
}

func (c *gateClock) advance(d time.Duration) { c.now = c.now.Add(d) }

// fire pops the oldest pending timer and runs its callback.
func (c *gateClock) fire(t *testing.T) {
	t.Helper()
	if len(c.timers) == 0 {
		t.Fatal("no pending gate timer to fire")
	}
	timer := c.timers[0]
	c.timers = c.timers[1:]
	timer.fn()
}

func newGateForTest(clock *gateClock, probe func(context.Context) error) *usageProbeGate {
	gate := newUsageProbeGate(probe, context.Background)
	gate.now = func() time.Time { return clock.now }
	gate.afterFunc = func(d time.Duration, fn func()) {
		clock.timers = append(clock.timers, gateTimer{wait: d, fn: fn})
	}
	return gate
}

// A burst of triggers during an in-flight probe — the modern Claude CLI emits
// several EventTurnComplete per logical turn — must collapse into exactly one
// trailing run, scheduled a full cooldown after the in-flight probe started.
func TestUsageProbeGateBurstCoalescesIntoOneTrailingRun(t *testing.T) {
	clock := newGateClock()
	var gate *usageProbeGate
	calls := 0
	gate = newGateForTest(clock, func(context.Context) error {
		calls++
		if calls == 1 {
			gate.Request()
			gate.Request()
			gate.Request()
		}
		return nil
	})

	gate.Request()

	if calls != 1 {
		t.Fatalf("probe calls = %d, want 1 (burst must not run inline)", calls)
	}
	if len(clock.timers) != 1 {
		t.Fatalf("pending timers = %d, want 1 trailing run", len(clock.timers))
	}
	if got := clock.timers[0].wait; got != usageProbeMinInterval {
		t.Fatalf("trailing wait = %v, want %v", got, usageProbeMinInterval)
	}

	clock.advance(usageProbeMinInterval)
	clock.fire(t)
	if calls != 2 {
		t.Fatalf("probe calls after trailing fire = %d, want 2", calls)
	}
	if len(clock.timers) != 0 {
		t.Fatalf("pending timers after trailing run = %d, want 0", len(clock.timers))
	}
}

// Triggers inside the cooldown collapse into one scheduled run: the first
// arms a timer for the cooldown remainder, later ones are absorbed by it.
func TestUsageProbeGateCooldownCollapsesRequestsIntoOneTimer(t *testing.T) {
	clock := newGateClock()
	calls := 0
	gate := newGateForTest(clock, func(context.Context) error { calls++; return nil })

	gate.Request()
	clock.advance(10 * time.Second)
	gate.Request()
	gate.Request()

	if calls != 1 {
		t.Fatalf("probe calls = %d, want 1 (cooldown must defer, not run)", calls)
	}
	if len(clock.timers) != 1 {
		t.Fatalf("pending timers = %d, want 1", len(clock.timers))
	}
	if got, want := clock.timers[0].wait, usageProbeMinInterval-10*time.Second; got != want {
		t.Fatalf("scheduled wait = %v, want the cooldown remainder %v", got, want)
	}

	clock.advance(20 * time.Second)
	clock.fire(t)
	if calls != 2 {
		t.Fatalf("probe calls after cooldown fire = %d, want 2", calls)
	}
}

// A probe answered 429 with Retry-After holds automatic probing for exactly
// that long, even past the cooldown, and a timer that fires while the backoff
// still holds re-arms instead of probing.
func TestUsageProbeGateBackoffFromRateLimitedProbe(t *testing.T) {
	clock := newGateClock()
	calls := 0
	probeErr := error(&claude.RateLimitedError{RetryAfter: 45 * time.Second})
	gate := newGateForTest(clock, func(context.Context) error { calls++; return probeErr })

	gate.Request()
	if calls != 1 {
		t.Fatalf("probe calls = %d, want 1", calls)
	}
	if got := gate.BackoffRemaining(); got != 45*time.Second {
		t.Fatalf("BackoffRemaining = %v, want 45s", got)
	}

	// Cooldown fully elapsed; only the backoff still holds.
	clock.advance(usageProbeMinInterval)
	if got := gate.BackoffRemaining(); got != 15*time.Second {
		t.Fatalf("BackoffRemaining after 30s = %v, want 15s", got)
	}
	gate.Request()
	if calls != 1 {
		t.Fatalf("probe calls during backoff = %d, want 1", calls)
	}
	if len(clock.timers) != 1 || clock.timers[0].wait != 15*time.Second {
		t.Fatalf("timers = %+v, want one at the 15s backoff remainder", clock.timers)
	}

	// Fire without advancing: backoff still holds, so it must re-arm.
	clock.fire(t)
	if calls != 1 {
		t.Fatalf("probe calls after early fire = %d, want 1", calls)
	}
	if len(clock.timers) != 1 || clock.timers[0].wait != 15*time.Second {
		t.Fatalf("timers after early fire = %+v, want one re-armed at 15s", clock.timers)
	}

	probeErr = nil
	clock.advance(15 * time.Second)
	clock.fire(t)
	if calls != 2 {
		t.Fatalf("probe calls after backoff expiry = %d, want 2", calls)
	}
	if got := gate.BackoffRemaining(); got != 0 {
		t.Fatalf("BackoffRemaining after clean probe = %v, want 0", got)
	}
}

// A 429 without a usable Retry-After falls back to the default backoff.
func TestUsageProbeGateDefaultBackoffWithoutRetryAfter(t *testing.T) {
	clock := newGateClock()
	gate := newGateForTest(clock, func(context.Context) error {
		return &claude.RateLimitedError{}
	})

	gate.Request()
	if got := gate.BackoffRemaining(); got != defaultUsageProbeBackoff {
		t.Fatalf("BackoffRemaining = %v, want the %v default", got, defaultUsageProbeBackoff)
	}
}

// NoteResult lets the manual refresh path report its outcome: a wrapped 429
// starts a backoff that holds automatic probes, while nil and ordinary errors
// change nothing.
func TestUsageProbeGateNoteResultFeedsExternalOutcome(t *testing.T) {
	clock := newGateClock()
	calls := 0
	gate := newGateForTest(clock, func(context.Context) error { calls++; return nil })

	gate.NoteResult(nil)
	gate.NoteResult(errors.New("connection reset"))
	if got := gate.BackoffRemaining(); got != 0 {
		t.Fatalf("BackoffRemaining after non-429 outcomes = %v, want 0", got)
	}

	gate.NoteResult(fmt.Errorf("manual refresh: %w", &claude.RateLimitedError{RetryAfter: 90 * time.Second}))
	if got := gate.BackoffRemaining(); got != 90*time.Second {
		t.Fatalf("BackoffRemaining after wrapped 429 = %v, want 90s", got)
	}

	gate.Request()
	if calls != 0 {
		t.Fatalf("probe calls during externally-reported backoff = %d, want 0", calls)
	}
	if len(clock.timers) != 1 || clock.timers[0].wait != 90*time.Second {
		t.Fatalf("timers = %+v, want one at the 90s backoff", clock.timers)
	}
}
