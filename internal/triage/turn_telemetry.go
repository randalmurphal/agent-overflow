package triage

import (
	"context"
	"errors"

	"agent-overflow/internal/provider"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

type turnOutcomeKind uint8

const (
	turnOutcomeCompleted turnOutcomeKind = iota
	turnOutcomeProviderError
	turnOutcomeInterrupted
	turnOutcomePersistenceFailure
	turnOutcomeCleanup
)

type turnOutcome struct {
	kind        turnOutcomeKind
	description string
}

func completedTurnOutcome(meta turnCompleteMeta, persistErr error) turnOutcome {
	if persistErr != nil {
		return turnOutcome{kind: turnOutcomePersistenceFailure, description: persistErr.Error()}
	}
	if meta.Aborted {
		return turnOutcome{kind: turnOutcomeInterrupted, description: firstNonEmptyOutcomeDescription(meta.Error, "turn aborted")}
	}
	if meta.Truncated {
		return turnOutcome{kind: turnOutcomeInterrupted, description: firstNonEmptyOutcomeDescription(meta.Error, "turn truncated")}
	}
	switch canonicalStopReason(meta) {
	case "interrupted":
		return turnOutcome{kind: turnOutcomeInterrupted, description: firstNonEmptyOutcomeDescription(meta.Error, "turn interrupted")}
	case "error":
		return turnOutcome{kind: turnOutcomeProviderError, description: firstNonEmptyOutcomeDescription(meta.Error, "provider turn failed")}
	}
	if meta.Error != "" {
		return turnOutcome{kind: turnOutcomeProviderError, description: meta.Error}
	}
	return turnOutcome{kind: turnOutcomeCompleted}
}

func firstNonEmptyOutcomeDescription(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func providerErrorTurnOutcome(description string) turnOutcome {
	return turnOutcome{
		kind:        turnOutcomeProviderError,
		description: firstNonEmptyOutcomeDescription(description, "provider turn failed"),
	}
}

func cleanupTurnOutcome() turnOutcome {
	return turnOutcome{kind: turnOutcomeCleanup, description: "turn ended during thread cleanup"}
}

func (o turnOutcome) errored() bool {
	return o.kind != turnOutcomeCompleted
}

// openTurnSpan begins a turn.lifecycle span for the incoming turn. Any
// existing span for the thread is closed first because a provider can re-send
// EventTurnStart after reinitializing. The replacement is a lifecycle
// transition, not a terminal turn outcome, so it does not increment either
// terminal counter.
func (r *Router) openTurnSpan(evt provider.ProviderEvent, turnIndex int) {
	r.mu.Lock()
	tracer := r.tracer
	generation := r.turnSpanGenerations[evt.ThreadID] + 1
	r.turnSpanGenerations[evt.ThreadID] = generation
	existing := r.turnSpans[evt.ThreadID]
	if existing != nil {
		delete(r.turnSpans, evt.ThreadID)
	}
	r.mu.Unlock()
	if existing != nil {
		existing.End()
	}
	thread, err := r.store.GetThread(evt.ThreadID)
	if err != nil {
		// Provider and model attributes require the thread row. Dropping the
		// span is preferable to exporting a span with misleading identity.
		return
	}
	_, span := tracer.Start(context.Background(), "turn.lifecycle",
		trace.WithAttributes(
			attribute.String("thread.id", evt.ThreadID),
			attribute.String("provider", thread.Provider),
			attribute.String("model", thread.Model),
			attribute.Int("turn.index", turnIndex),
		),
	)
	r.mu.Lock()
	if r.turnSpanGenerations[evt.ThreadID] != generation {
		r.mu.Unlock()
		span.End()
		return
	}
	r.turnSpans[evt.ThreadID] = span
	r.mu.Unlock()
	r.metrics.TurnsStarted.Add(context.Background(), 1,
		metric.WithAttributes(attribute.String("provider", thread.Provider)))
}

// finishTurnSpan atomically claims the thread's active span. Exactly one
// terminal path records an outcome even when a synthetic completion races a
// later provider completion.
func (r *Router) finishTurnSpan(threadID string, outcome turnOutcome) {
	r.mu.Lock()
	r.turnSpanGenerations[threadID]++
	span, ok := r.turnSpans[threadID]
	if ok {
		delete(r.turnSpans, threadID)
	}
	r.mu.Unlock()
	if !ok {
		return
	}
	r.recordTurnSpanOutcome(span, outcome)
}

// recordTurnSpanOutcome is also used by cleanup after it detaches an orphan
// span while holding the router lock. Recording and End stay outside that
// lock because an exporter callback may acquire unrelated application locks.
func (r *Router) recordTurnSpanOutcome(span trace.Span, outcome turnOutcome) {
	if outcome.errored() {
		err := errors.New(outcome.description)
		span.RecordError(err)
		span.SetStatus(codes.Error, outcome.description)
		r.metrics.TurnsErrored.Add(context.Background(), 1)
	} else {
		r.metrics.TurnsCompleted.Add(context.Background(), 1)
	}
	span.End()
}
