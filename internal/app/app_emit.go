package app

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
	"agent-overflow/internal/transport"
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
	a.emitKeyed(name, data)
}

// entityFilteredChannels is the set of channels whose frames are addressed
// per entity on the wire (transport's EntityFiltered column). Built once
// from transport's own list rather than restated, so a row added there
// starts having its key derived here with no second edit.
var entityFilteredChannels = func() map[string]bool {
	names := transport.EntityFilteredChannels()
	set := make(map[string]bool, len(names))
	for _, name := range names {
		set[name] = true
	}
	return set
}()

// emitKeyed is emit plus the entity key it derived, returned so the one
// caller that needs the same value (emitWithReplay) does not derive it a
// second time.
//
// The derivation is a reflect walk with a JSON round-trip fallback
// (internal/eventscope), which is why it is conditional and why it happens
// exactly once. Two consumers want it and they want it for different frames:
// the transport bus needs it on EntityFiltered channels so a watching
// connection can be narrowed, and the NDJSON replay log needs it on EVERY
// thread-scoped channel. The condition is therefore their union, not the
// bus's half — narrowing it to EntityFiltered channels alone would silently
// stop attributing replay records for everything else, which is the whole
// content of that log.
func (a *App) emitKeyed(name eventchan.Channel, data any) string {
	a.rememberRateLimitsEvent(name, data)
	// The OS-notification mapping taps the same funnel: four moments are
	// worth interrupting a person for and all four are announced here
	// (app_notification_mapping.go). It projects and queues; it never
	// blocks this goroutine.
	a.tapNotification(name, data)
	entityKey := ""
	if entityFilteredChannels[string(name)] || (a.replay != nil && a.replay.Enabled()) {
		entityKey = eventscope.ThreadIDFromEvent(data)
	}
	// Snapshot the bus pointer once so a concurrent SetEventBus cannot
	// flip nil/non-nil between the guard and the Emit call. Deliberately
	// AFTER the derivation: the replay log is written by a caller of this
	// function and does not depend on a bus existing, so bailing first
	// would stop attributing replay records during the pre-Startup window.
	bus := a.eventBus.Load()
	if bus == nil && a.testEmitHook == nil {
		return entityKey
	}
	if bus != nil {
		if _, err := bus.EmitEntity(name, entityKey, data); err != nil {
			// json.Marshal failure on a payload we own — log and drop.
			// The bus is best-effort by design (drops on full subscriber
			// channels) so we don't propagate an error to callers.
			log.Printf("emit: bus marshal %s: %v", name, err)
		}
	}
	if a.testEmitHook != nil {
		a.testEmitHook(string(name), data)
	}
	return entityKey
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
	a.providerLifecycleService().EmitSnapshot(snap)
}

// emitWithReplay returns an event emitter that both publishes to the
// transport bus and mirrors the event into the per-thread replay log
// when the event is thread-scoped. We inspect the payload for a
// `threadId` field so we don't introduce a hard dependency on any
// single event shape.
//
// The emission goes through a.emitKeyed so the bus stamps its per-channel
// seq; the replay log receives the same payload because the replay
// format records provider events, not wire envelopes.
//
// The thread id comes BACK from that call rather than being extracted
// again. There is one derivation per emit and this is the only place that
// could have made it two — the bus wants the same value for entity
// addressing, and eventscope's lookup ends in a JSON round trip for the
// anonymous-struct payloads that dominate this funnel.
func (a *App) emitWithReplay() func(eventchan.Channel, any) {
	return func(eventName eventchan.Channel, data any) {
		threadID := a.emitKeyed(eventName, data)
		if a.replay == nil || !a.replay.Enabled() {
			return
		}
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
