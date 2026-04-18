package triage

import (
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// newTestRouterWithSpans wires a Router to an in-memory SQLite store and a
// tracetest recorder so tests can assert span lifecycle.
func newTestRouterWithSpans(t *testing.T) (*Router, *tracetest.SpanRecorder, *store.Store, []provider.ProviderEvent) {
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

	var (
		mu     sync.Mutex
		emits  []provider.ProviderEvent
	)
	emit := func(name string, data any) {
		mu.Lock()
		defer mu.Unlock()
		if evt, ok := data.(provider.ProviderEvent); ok {
			emits = append(emits, evt)
		}
	}

	r := NewRouter(st, emit)
	recorder := tracetest.NewSpanRecorder()
	tp := trace.NewTracerProvider(trace.WithSpanProcessor(recorder))
	r.SetTelemetry(tp.Tracer("test"), TurnMetrics{})

	return r, recorder, st, emits
}

func TestTurnSpanOpensOnStart(t *testing.T) {
	r, recorder, _, _ := newTestRouterWithSpans(t)

	if err := r.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "thread-1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("Handle TurnStart: %v", err)
	}

	// Span is still open; tracetest only records ended spans.
	if len(recorder.Ended()) != 0 {
		t.Errorf("Ended() = %d, want 0 before TurnComplete", len(recorder.Ended()))
	}
}

func TestTurnSpanClosesOnComplete(t *testing.T) {
	r, recorder, _, _ := newTestRouterWithSpans(t)

	if err := r.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "thread-1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("Handle TurnStart: %v", err)
	}
	if err := r.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnComplete,
		ThreadID:  "thread-1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("Handle TurnComplete: %v", err)
	}

	spans := recorder.Ended()
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
}

func TestTurnSpanReSentTurnStartClosesPrevious(t *testing.T) {
	// Claude re-sends system.init after an interrupt, which surfaces as a
	// second EventTurnStart for the same thread. The router must close the
	// previous span before starting a new one to avoid leaking spans.
	r, recorder, _, _ := newTestRouterWithSpans(t)

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
	if got := len(recorder.Ended()); got != 2 {
		t.Errorf("Ended() = %d, want 2 (one per re-init)", got)
	}

	if err := r.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnComplete,
		ThreadID:  "thread-1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("Handle TurnComplete: %v", err)
	}
	if got := len(recorder.Ended()); got != 3 {
		t.Errorf("Ended() after complete = %d, want 3", got)
	}
}

func TestCleanupThreadEndsLiveSpan(t *testing.T) {
	r, recorder, _, _ := newTestRouterWithSpans(t)

	if err := r.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "thread-1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("Handle TurnStart: %v", err)
	}

	r.CleanupThread("thread-1")

	if got := len(recorder.Ended()); got != 1 {
		t.Errorf("Ended() after CleanupThread = %d, want 1", got)
	}
}

func TestTurnSpanRecordsErrorOnPersistFailure(t *testing.T) {
	r, recorder, st, _ := newTestRouterWithSpans(t)

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

	// Close the store so persistTurnText fails; we still want the span to close.
	_ = st.Close()

	if err := r.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnComplete,
		ThreadID:  "thread-1",
		Timestamp: time.Now(),
	}); err == nil {
		t.Fatal("expected TurnComplete to return an error after store close")
	}

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("Ended() = %d, want 1", len(spans))
	}
	events := spans[0].Events()
	sawError := false
	for _, ev := range events {
		if strings.HasPrefix(ev.Name, "exception") {
			sawError = true
			break
		}
	}
	if !sawError {
		t.Errorf("span did not record an error event; events=%+v", events)
	}
}

// newRouterForUUID just suppresses "unused import" linting for uuid in
// environments where the compiler strips it. Real tests above construct
// threads via store.Thread literals with uuid-independent IDs.
func newRouterForUUID() string { return uuid.NewString() }
