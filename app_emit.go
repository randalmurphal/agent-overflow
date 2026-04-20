package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/observability/replay"
)

// SeqEnvelope is the shape every event takes on the Wails wire. The
// Go→frontend boundary in a.emit wraps the caller's payload into this
// envelope so the frontend can log seq gaps. The replay log takes the
// un-enveloped payload because its format records provider events, not
// wire envelopes.
//
// Keeping the nesting (rather than mutating arbitrary payload structs
// in place) lets every caller keep using its existing Go type and
// lets the frontend deserialise a stable `{seq, data}` shape. Wails'
// CustomEvent runs `json.Marshal(&envelope)` which produces
// `{"seq": N, "data": <payload>}`.
type SeqEnvelope struct {
	Seq  uint64 `json:"seq"`
	Data any    `json:"data"`
}

// emit stamps a monotonic seq on every Wails event and forwards the
// wrapped envelope through the Wails event bus. The frontend's
// wrapEventOn helper peels the envelope back off — subscribers see the
// same Go payload shape they would have without the envelope.
//
// When a.app is nil AND no test hook is installed, the helper is a
// silent no-op — this matches the pre-Shutdown boot path (Wails may
// not be initialised) and keeps tests free to construct an App with
// just the fields they need. Tests that want to observe the envelope
// install testEmitHook.
func (a *App) emit(name string, data any) {
	if a.app == nil && a.testEmitHook == nil {
		return
	}
	seq := a.seq.Add(1)
	env := SeqEnvelope{Seq: seq, Data: data}
	if a.app != nil {
		a.app.Event.Emit(name, env)
	}
	if a.testEmitHook != nil {
		a.testEmitHook(name, env)
	}
}

// emitWithReplay returns an event emitter that both pushes to the Wails
// frontend and mirrors the event into the per-thread replay log when the
// event is thread-scoped. We inspect the payload for a `threadId` field so
// we don't introduce a hard dependency on any single event shape.
//
// The emission goes through a.emit so every event gets the seq envelope;
// the replay log receives the raw (un-enveloped) payload because the
// replay format records provider events, not Wails wire envelopes.
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
				errs = appendError(errs, wrapLifecycleError("close "+r.label, r.err))
			}
		case <-deadline:
			for label := range pending {
				errs = appendError(errs, fmt.Errorf("close %s: did not finish within %s", label, timeout))
			}
			return errs
		}
	}
	return errs
}

