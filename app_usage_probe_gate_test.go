package main

import (
	"context"
	"testing"
	"time"
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

// Server 429 backoffs are per-account state and live in usageBackoffLedger
// (app_usage_backoff_test.go), not here — the gate only coalesces and paces.
