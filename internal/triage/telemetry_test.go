package triage

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type turnTelemetryProbe struct {
	spans   *tracetest.SpanRecorder
	metrics *sdkmetric.ManualReader
}

type firstSpanStartBlocker struct {
	blocked atomic.Bool
	started chan struct{}
	release chan struct{}
}

func newFirstSpanStartBlocker() *firstSpanStartBlocker {
	return &firstSpanStartBlocker{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (b *firstSpanStartBlocker) OnStart(context.Context, trace.ReadWriteSpan) {
	if b.blocked.CompareAndSwap(false, true) {
		close(b.started)
		<-b.release
	}
}

func (*firstSpanStartBlocker) OnEnd(trace.ReadOnlySpan) {}

func (*firstSpanStartBlocker) Shutdown(context.Context) error { return nil }

func (*firstSpanStartBlocker) ForceFlush(context.Context) error { return nil }

// newTestRouterWithTelemetry wires the same raw tracer and metric instruments
// production injects from app_startup.go. Assertions therefore cover the live
// Router path rather than a parallel helper API.
func newTestRouterWithTelemetry(t *testing.T) (*Router, *turnTelemetryProbe, *store.Store) {
	t.Helper()

	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Pre-seed a project + thread so span attribute lookups succeed.
	ensureTriageProject(t, st)
	thread := store.Thread{
		ID:            "thread-1",
		ProjectID:     triageTestProjectID,
		Title:         "t",
		Provider:      "claude",
		WorkspacePath: "/tmp/triage",
		Model:         "claude-sonnet-4-6",
		Mode:          "chat",
		CreatedAt:     time.Now().UnixMilli(),
		UpdatedAt:     time.Now().UnixMilli(),
	}
	if err := st.CreateThread(thread); err != nil {
		t.Fatalf("seed thread: %v", err)
	}

	r := NewRouter(st, func(eventchan.Channel, any) {})
	recorder := tracetest.NewSpanRecorder()
	tp := trace.NewTracerProvider(trace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	meter := mp.Meter("triage/telemetry-test")
	turnsStarted, err := meter.Int64Counter("turns.started")
	if err != nil {
		t.Fatalf("turns.started counter: %v", err)
	}
	turnsCompleted, err := meter.Int64Counter("turns.completed")
	if err != nil {
		t.Fatalf("turns.completed counter: %v", err)
	}
	turnsErrored, err := meter.Int64Counter("turns.errored")
	if err != nil {
		t.Fatalf("turns.errored counter: %v", err)
	}
	r.SetTelemetry(tp.Tracer("test"), TurnMetrics{
		TurnsStarted:   turnsStarted,
		TurnsCompleted: turnsCompleted,
		TurnsErrored:   turnsErrored,
	})

	return r, &turnTelemetryProbe{spans: recorder, metrics: reader}, st
}

func collectMetricCounters(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	values := make(map[string]int64)
	for _, scope := range collected.ScopeMetrics {
		for _, got := range scope.Metrics {
			sum, ok := got.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, point := range sum.DataPoints {
				values[got.Name] += point.Value
			}
		}
	}
	return values
}

func assertTurnMetrics(t *testing.T, reader *sdkmetric.ManualReader, started, completed, errored int64) {
	t.Helper()
	want := map[string]int64{
		"turns.started":   started,
		"turns.completed": completed,
		"turns.errored":   errored,
	}
	got := collectMetricCounters(t, reader)
	for name, expected := range want {
		if got[name] != expected {
			t.Errorf("%s = %d, want %d", name, got[name], expected)
		}
	}
}

func requireErroredSpan(t *testing.T, span trace.ReadOnlySpan, description string) {
	t.Helper()
	if got := span.Status().Code; got != codes.Error {
		t.Errorf("span status = %v, want Error", got)
	}
	if description != "" && span.Status().Description != description {
		t.Errorf("span status description = %q, want %q", span.Status().Description, description)
	}
	for _, event := range span.Events() {
		if strings.HasPrefix(event.Name, "exception") {
			return
		}
	}
	t.Errorf("span did not record an exception event; events=%+v", span.Events())
}

func TestTurnSpanOpensOnStart(t *testing.T) {
	r, telemetry, _ := newTestRouterWithTelemetry(t)

	if err := r.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "thread-1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("Handle TurnStart: %v", err)
	}

	// Span is still open; tracetest only records ended spans.
	if len(telemetry.spans.Ended()) != 0 {
		t.Errorf("Ended() = %d, want 0 before TurnComplete", len(telemetry.spans.Ended()))
	}
	assertTurnMetrics(t, telemetry.metrics, 1, 0, 0)
}

func TestTurnSpanClosesOnComplete(t *testing.T) {
	r, telemetry, _ := newTestRouterWithTelemetry(t)

	if err := r.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "thread-1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("Handle TurnStart: %v", err)
	}
	if err := r.Handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     "thread-1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("Handle TurnComplete: %v", err)
	}

	spans := telemetry.spans.Ended()
	if len(spans) != 1 {
		t.Fatalf("Ended() = %d, want 1", len(spans))
	}
	s := spans[0]
	if s.Name() != "turn.lifecycle" {
		t.Errorf("span name = %q, want turn.lifecycle", s.Name())
	}

	attrs := make(map[string]attribute.Value)
	for _, kv := range s.Attributes() {
		attrs[string(kv.Key)] = kv.Value
	}
	if v, ok := attrs["thread.id"]; !ok || v.AsString() != "thread-1" {
		t.Errorf("thread.id = %v, want thread-1", attrs["thread.id"])
	}
	if v, ok := attrs["provider"]; !ok || v.AsString() != "claude" {
		t.Errorf("provider = %v, want claude", attrs["provider"])
	}
	if v, ok := attrs["model"]; !ok || v.AsString() != "claude-sonnet-4-6" {
		t.Errorf("model = %v, want claude-sonnet-4-6", attrs["model"])
	}
	if v, ok := attrs["turn.index"]; !ok || v.AsInt64() != 0 {
		t.Errorf("turn.index = %v, want 0", attrs["turn.index"])
	}
	if got := s.Status().Code; got != codes.Unset {
		t.Errorf("span status = %v, want Unset", got)
	}
	assertTurnMetrics(t, telemetry.metrics, 1, 1, 0)
}

func TestTurnSpanReSentTurnStartClosesPrevious(t *testing.T) {
	// Claude re-sends system.init after an interrupt, which surfaces as a
	// second EventTurnStart for the same thread. The router must close the
	// previous span before starting a new one to avoid leaking spans.
	r, telemetry, _ := newTestRouterWithTelemetry(t)

	for i := 0; i < 3; i++ {
		if err := r.Handle(provider.ProviderEvent{
			Kind:      provider.EventTurnStart,
			ThreadID:  "thread-1",
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("Handle TurnStart %d: %v", i, err)
		}
	}

	// Two previous spans should have ended; the third is still live.
	if got := len(telemetry.spans.Ended()); got != 2 {
		t.Errorf("Ended() = %d, want 2 (one per re-init)", got)
	}

	if err := r.Handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     "thread-1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("Handle TurnComplete: %v", err)
	}
	if got := len(telemetry.spans.Ended()); got != 3 {
		t.Errorf("Ended() after complete = %d, want 3", got)
	}
	assertTurnMetrics(t, telemetry.metrics, 3, 1, 0)
}

func TestCleanupThreadSynthesizesTruncatedOutcome(t *testing.T) {
	r, telemetry, _ := newTestRouterWithTelemetry(t)

	if err := r.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "thread-1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("Handle TurnStart: %v", err)
	}

	r.CleanupThread("thread-1")

	spans := telemetry.spans.Ended()
	if got := len(spans); got != 1 {
		t.Errorf("Ended() after CleanupThread = %d, want 1", got)
		return
	}
	requireErroredSpan(t, spans[0], "turn truncated")
	assertTurnMetrics(t, telemetry.metrics, 1, 0, 1)
}

func TestTurnSpanRecordsErrorOnPersistFailure(t *testing.T) {
	r, telemetry, st := newTestRouterWithTelemetry(t)

	if err := r.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "thread-1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("Handle TurnStart: %v", err)
	}

	// Accumulate some text so handleTurnComplete attempts a persist.
	_ = r.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "thread-1",
		Content:   "hello",
		Timestamp: time.Now(),
	})

	// Close the store so the text-delta persistence path fails; we still
	// want the span to close.
	_ = st.Close()

	if err := r.Handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     "thread-1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err == nil {
		t.Fatal("expected TurnComplete to return an error after store close")
	}

	spans := telemetry.spans.Ended()
	if len(spans) != 1 {
		t.Fatalf("Ended() = %d, want 1", len(spans))
	}
	requireErroredSpan(t, spans[0], "")
	assertTurnMetrics(t, telemetry.metrics, 1, 0, 1)
}

func TestTurnTelemetryClassifiesTerminalOutcomes(t *testing.T) {
	r, telemetry, _ := newTestRouterWithTelemetry(t)

	outcomes := []struct {
		name        string
		turnIndex   int
		complete    provider.TurnCompleteMeta
		description string
	}{
		{
			name:        "provider error",
			turnIndex:   1,
			complete:    &provider.WireTurnCompleteMeta{StopReason: "error", ErrorMessage: "provider failed"},
			description: "provider failed",
		},
		{
			name:        "aborted",
			turnIndex:   2,
			complete:    &provider.WireTurnCompleteMeta{StopReason: "interrupted", Aborted: true},
			description: "turn aborted",
		},
		{
			name:        "truncated",
			turnIndex:   3,
			complete:    &provider.TruncatedTurnCompleteMeta{},
			description: "turn truncated",
		},
	}

	for _, outcome := range outcomes {
		if err := r.Handle(provider.ProviderEvent{
			Kind:      provider.EventTurnStart,
			ThreadID:  "thread-1",
			TurnIndex: outcome.turnIndex,
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("%s start: %v", outcome.name, err)
		}
		if err := r.Handle(provider.ProviderEvent{
			Kind:         provider.EventTurnComplete,
			ThreadID:     "thread-1",
			TurnIndex:    outcome.turnIndex,
			TurnComplete: outcome.complete,
			Timestamp:    time.Now(),
		}); err != nil {
			t.Fatalf("%s complete: %v", outcome.name, err)
		}
	}

	spans := telemetry.spans.Ended()
	if len(spans) != len(outcomes) {
		t.Fatalf("Ended() = %d, want %d", len(spans), len(outcomes))
	}
	for i, outcome := range outcomes {
		t.Run(outcome.name, func(t *testing.T) {
			requireErroredSpan(t, spans[i], outcome.description)
		})
	}
	assertTurnMetrics(t, telemetry.metrics, 3, 0, 3)
}

func TestCleanupThreadClassifiesDetachedOrphanSpan(t *testing.T) {
	r, telemetry, _ := newTestRouterWithTelemetry(t)
	if err := r.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "thread-1",
		TurnIndex: 1,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	// Simulate the defensive cleanup state: the live turn/round bookkeeping
	// is already gone, but its span still needs a terminal classification.
	r.clearOpenTurn("thread-1")
	r.takeOpenRound("thread-1")
	r.CleanupThread("thread-1")

	spans := telemetry.spans.Ended()
	if len(spans) != 1 {
		t.Fatalf("Ended() = %d, want 1", len(spans))
	}
	requireErroredSpan(t, spans[0], "turn ended during thread cleanup")
	assertTurnMetrics(t, telemetry.metrics, 1, 0, 1)
}

func TestFatalProviderErrorFinishesSpanOnce(t *testing.T) {
	r, telemetry, _ := newTestRouterWithTelemetry(t)
	if err := r.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "thread-1",
		TurnIndex: 1,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	if err := r.Handle(provider.ProviderEvent{
		Kind:      provider.EventError,
		ThreadID:  "thread-1",
		Content:   "provider process failed",
		Meta:      json.RawMessage(`{"fatal":true}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("fatal provider error: %v", err)
	}

	spans := telemetry.spans.Ended()
	if len(spans) != 1 {
		t.Fatalf("Ended() = %d, want 1", len(spans))
	}
	requireErroredSpan(t, spans[0], "provider process failed")
	// The fatal path also synthesizes a truncated completion. Claiming the
	// span before synthesis keeps that second terminal signal from counting.
	assertTurnMetrics(t, telemetry.metrics, 1, 0, 1)
}

func TestInterruptedQueuedPredecessorUsesErroredOutcome(t *testing.T) {
	r, telemetry, _ := newTestRouterWithTelemetry(t)
	if err := r.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "thread-1",
		TurnIndex: 1,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	r.settleQueuedEchoPredecessor("thread-1", 1, time.Now().UnixMilli(), "interrupted")

	spans := telemetry.spans.Ended()
	if len(spans) != 1 {
		t.Fatalf("Ended() = %d, want 1", len(spans))
	}
	requireErroredSpan(t, spans[0], "turn interrupted")
	assertTurnMetrics(t, telemetry.metrics, 1, 0, 1)
}

func TestQueuedPredecessorPersistenceFailureOverridesCompletionOutcome(t *testing.T) {
	r, telemetry, st := newTestRouterWithTelemetry(t)
	if err := r.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "thread-1",
		TurnIndex: 1,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	_ = st.Close()

	r.settleQueuedEchoPredecessor("thread-1", 1, time.Now().UnixMilli(), "end_turn")

	spans := telemetry.spans.Ended()
	if len(spans) != 1 {
		t.Fatalf("Ended() = %d, want 1", len(spans))
	}
	requireErroredSpan(t, spans[0], "")
	if description := spans[0].Status().Description; !strings.Contains(description, "database is closed") {
		t.Errorf("status description = %q, want persistence failure", description)
	}
	assertTurnMetrics(t, telemetry.metrics, 1, 0, 1)
}

func TestConcurrentTurnSpanStartCannotOverwriteNewerGeneration(t *testing.T) {
	r, telemetry, _ := newTestRouterWithTelemetry(t)
	blocker := newFirstSpanStartBlocker()
	recorder := tracetest.NewSpanRecorder()
	tp := trace.NewTracerProvider(
		trace.WithSpanProcessor(blocker),
		trace.WithSpanProcessor(recorder),
	)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	r.SetTelemetry(tp.Tracer("generation-test"), TurnMetrics{})

	firstDone := make(chan struct{})
	go func() {
		r.openTurnSpan(provider.ProviderEvent{ThreadID: "thread-1"}, 1)
		close(firstDone)
	}()
	<-blocker.started

	// The second start advances the generation and registers while the first
	// is still inside Tracer.Start. Releasing the first must end its stale span
	// rather than overwrite the newer map entry.
	r.openTurnSpan(provider.ProviderEvent{ThreadID: "thread-1"}, 2)
	close(blocker.release)
	<-firstDone

	r.mu.Lock()
	active := r.activeTurnSpanCountLocked()
	r.mu.Unlock()
	if active != 1 {
		t.Fatalf("active turn spans = %d, want the newer span only", active)
	}
	r.finishTurnSpan("thread-1", turnOutcome{kind: turnOutcomeCompleted})
	if got := len(recorder.Ended()); got != 2 {
		t.Fatalf("ended spans = %d, want stale plus completed current span", got)
	}
	assertTurnMetrics(t, telemetry.metrics, 1, 1, 0)
}

func TestCleanupInvalidatesSpanStartBeforeRegistration(t *testing.T) {
	r, telemetry, _ := newTestRouterWithTelemetry(t)
	blocker := newFirstSpanStartBlocker()
	recorder := tracetest.NewSpanRecorder()
	tp := trace.NewTracerProvider(
		trace.WithSpanProcessor(blocker),
		trace.WithSpanProcessor(recorder),
	)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	r.SetTelemetry(tp.Tracer("cleanup-generation-test"), TurnMetrics{})

	startDone := make(chan struct{})
	go func() {
		r.openTurnSpan(provider.ProviderEvent{ThreadID: "thread-1"}, 1)
		close(startDone)
	}()
	<-blocker.started
	r.CleanupThread("thread-1")
	r.MarkThreadActive("thread-1")
	r.openTurnSpan(provider.ProviderEvent{ThreadID: "thread-1"}, 2)
	close(blocker.release)
	<-startDone

	r.mu.Lock()
	active := r.activeTurnSpanCountLocked()
	r.mu.Unlock()
	if active != 1 {
		t.Fatalf("active turn spans = %d after cleanup and restart, want replacement only", active)
	}
	r.finishTurnSpan("thread-1", turnOutcome{kind: turnOutcomeCompleted})
	if got := len(recorder.Ended()); got != 2 {
		t.Fatalf("ended spans = %d, want stale start plus replacement", got)
	}
	assertTurnMetrics(t, telemetry.metrics, 1, 1, 0)
}
