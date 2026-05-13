package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/observability/replay"
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
// published.
func (a *App) emit(name string, data any) {
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
		a.testEmitHook(name, data)
	}
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
func (a *App) emitWithReplay() func(string, any) {
	return func(eventName string, data any) {
		a.emit(eventName, data)
		if a.replay == nil || !a.replay.Enabled() {
			return
		}
		threadID := threadIDFromEvent(data)
		if threadID == "" {
			return
		}
		rec, err := replay.NewRecord(time.Now(), threadID, eventName, data)
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
// Design-thread teardown runs synchronously in the goroutine that closed
// the session so each thread's state is cleaned up independently.
func closeSessionsParallel(a *App, sessions map[string]session, timeout time.Duration) []error {
	if len(sessions) == 0 {
		return nil
	}
	closers := make([]threadCloser, 0, len(sessions))
	for threadID, s := range sessions {
		closers = append(closers, sessionThreadCloser(a, threadID, s))
	}
	return runParallelClosers(closers, timeout)
}

// threadCloser is a single Close operation that runParallelClosers fires
// off in its own goroutine. The label is used to build a meaningful
// error message if Close fails or times out.
type threadCloser struct {
	label string
	close func() error
}

// sessionThreadCloser bundles the design teardown + provider Close for
// a single thread into one threadCloser so both run under the same
// parallel timeout.
func sessionThreadCloser(a *App, threadID string, s session) threadCloser {
	label := fmt.Sprintf("session for thread %s", threadID)
	return threadCloser{
		label: label,
		close: func() error {
			a.teardownDesignThread(threadID)
			providerSess := s.providerSession()
			if providerSess == nil {
				return nil
			}
			if err := providerSess.Close(); err != nil {
				return fmt.Errorf("close %s: %w", s.provider, err)
			}
			return nil
		},
	}
}

// runParallelClosers invokes every closer concurrently and collects their
// errors, enforcing a single wall-clock timeout across the whole set.
// Closers that do not finish in time are abandoned and reported as
// timeout errors.
func runParallelClosers(closers []threadCloser, timeout time.Duration) []error {
	if len(closers) == 0 {
		return nil
	}
	type result struct {
		label string
		err   error
	}
	results := make(chan result, len(closers))
	for _, c := range closers {
		go func(c threadCloser) {
			results <- result{c.label, c.close()}
		}(c)
	}

	var errs []error
	remaining := len(closers)
	deadline := time.After(timeout)
	pending := make(map[string]struct{}, len(closers))
	for _, c := range closers {
		pending[c.label] = struct{}{}
	}
	for remaining > 0 {
		select {
		case r := <-results:
			remaining--
			delete(pending, r.label)
			if r.err != nil {
				errs = errorsx.Append(errs, errorsx.WrapLifecycle("close "+r.label, r.err))
			}
		case <-deadline:
			for label := range pending {
				errs = errorsx.Append(errs, fmt.Errorf("close %s: did not finish within %s", label, timeout))
			}
			return errs
		}
	}
	return errs
}
