package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/closer"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/eventscope"
	"agent-overflow/internal/observability/replay"
	"agent-overflow/internal/provider"
)

// emit publishes an event onto the transport's event bus. The bus
// assigns a per-channel monotonic seq when it accepts the event; the
// frontend's WS client peels seq + data back out of the wire frame.
//
// When a.eventBus is unset AND no test hook is installed, the helper
// is a silent no-op — this matches the pre-Startup boot path (the bus
// may not be wired) and lets tests construct an App with just the
// fields they need. Tests that want to observe emissions install
// testEmitHook; it sees the same (name, data) pair the bus would have
// published, with the channel in its wire spelling.
//
// The channel is an eventchan.Channel, so a call site cannot name a
// channel the policy registry has never heard of without an explicit
// eventchan.Channel(...) conversion — which only the harness escape
// hatches (app_harness.go, app_harness_replay.go) spell.
func (a *App) emit(name eventchan.Channel, data any) {
	a.rememberRateLimitsEvent(name, data)
	// Snapshot the bus pointer once so a concurrent SetEventBus cannot
	// flip nil/non-nil between the guard and the Emit call.
	bus := a.eventBus.Load()
	if bus == nil && a.testEmitHook == nil {
		return
	}
	if bus != nil {
		if _, err := bus.Emit(name, data); err != nil {
			// json.Marshal failure on a payload we own — log and drop.
			// The bus is best-effort by design (drops on full subscriber
			// channels) so we don't propagate an error to callers.
			log.Printf("emit: bus marshal %s: %v", name, err)
		}
	}
	if a.testEmitHook != nil {
		a.testEmitHook(string(name), data)
	}
}

// emitRateLimitsSnapshot publishes an account-scoped rate-limit
// snapshot onto the `provider:usage` channel, matching the wire shape
// `triage.Router.handleRateLimits` produces for notification-driven
// updates. Both the Claude periodic probe loop and the Codex startup
// probe route through this helper so the two providers stay in lock-
// step on the action string ("rate_limits") and ThreadID convention
// (empty — rate limits are account-scoped and the frontend store keys by
// provider, account, limit ID, and duration).
//
// The shutting-down guard mirrors the Claude periodic-probe pattern
// — `a.emit` itself is a silent no-op once the bus is torn down, but
// returning early here also skips JSON marshalling and the test-hook
// fan-out for an emission that has nowhere to land.
func (a *App) emitRateLimitsSnapshot(snap provider.RateLimitsSnapshot) {
	if a.shuttingDown.Load() {
		return
	}
	if snap.AccountID == "" && a.providerAccounts != nil {
		if account, ok := a.providerAccounts.Active(snap.Provider, time.Now()); ok {
			snap.AccountID = account.ID
		}
	}
	a.emit(eventchan.ProviderUsage, provider.UsageEvent{
		Action:     "rate_limits",
		RateLimits: &snap,
	})
}

// emitWithReplay returns an event emitter that both publishes to the
// transport bus and mirrors the event into the per-thread replay log
// when the event is thread-scoped. We inspect the payload for a
// `threadId` field so we don't introduce a hard dependency on any
// single event shape.
//
// The emission goes through a.emit so the bus stamps its per-channel
// seq; the replay log receives the same payload because the replay
// format records provider events, not wire envelopes.
func (a *App) emitWithReplay() func(eventchan.Channel, any) {
	return func(eventName eventchan.Channel, data any) {
		a.emit(eventName, data)
		if a.replay == nil || !a.replay.Enabled() {
			return
		}
		threadID := eventscope.ThreadIDFromEvent(data)
		if threadID == "" {
			return
		}
		rec, err := replay.NewRecord(time.Now(), threadID, eventName.String(), data)
		if err != nil {
			log.Printf("replay: NewRecord failed: %v", err)
			return
		}
		if a.replay.Enqueue(rec) {
			if telemetryMetrics := a.telemetry.Metrics(); telemetryMetrics.ReplayEventsQueued != nil {
				telemetryMetrics.ReplayEventsQueued.Add(context.Background(), 1)
			}
		}
	}
}

// closeSessionsParallel closes every session concurrently, bounded by the
// given timeout. Any session whose Close does not return in time is
// abandoned — the teardown emits a timeout error for it and moves on.
func closeSessionsParallel(a *App, sessions map[string]session, timeout time.Duration) []error {
	if len(sessions) == 0 {
		return nil
	}
	tasks := make([]closer.Task, 0, len(sessions))
	for threadID, s := range sessions {
		tasks = append(tasks, sessionCloseTask(a, threadID, s))
	}
	return closer.RunParallel(tasks, timeout)
}

// sessionCloseTask bundles provider Close for one thread into a closer.Task.
func sessionCloseTask(a *App, threadID string, s session) closer.Task {
	return closer.Task{
		Label: fmt.Sprintf("session for thread %s", threadID),
		Close: func() error {
			// Routes through closeProviderSession so the orphan-reaper
			// release fires on a clean shutdown close, same as every other
			// teardown path.
			return a.closeProviderSession(threadID, s)
		},
	}
}
