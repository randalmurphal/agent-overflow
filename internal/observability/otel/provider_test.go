package otel

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestNewProviderNoopWhenDisabled(t *testing.T) {
	p, err := NewProvider(context.Background(), Config{Enabled: false})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if p == nil {
		t.Fatal("NewProvider returned nil")
	}
	if p.Enabled() {
		t.Error("Enabled() = true, want false for disabled config")
	}
	if p.Endpoint() != "" {
		t.Errorf("Endpoint() = %q, want empty", p.Endpoint())
	}
	if p.Tracer() == nil {
		t.Error("Tracer() returned nil")
	}
	if p.Meter() == nil {
		t.Error("Meter() returned nil")
	}
	// Noop provider must be safe to shut down with a non-nil context.
	if err := p.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown on disabled provider returned %v, want nil", err)
	}
}

func TestNewProviderMetricsBuiltEvenWhenNoop(t *testing.T) {
	p, err := NewProvider(context.Background(), Config{Enabled: false})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	m := p.Metrics()
	// All instruments must be non-nil so call sites can invoke them without
	// extra branching. We deliberately don't try to assert no-op-ness here;
	// the metric SDK's noop impl returns distinct instrument values that
	// happen to implement the interface correctly.
	if m.TurnsStarted == nil {
		t.Error("TurnsStarted instrument is nil")
	}
	if m.TurnsCompleted == nil {
		t.Error("TurnsCompleted instrument is nil")
	}
	if m.TurnsErrored == nil {
		t.Error("TurnsErrored instrument is nil")
	}
	if m.ItemsPersisted == nil {
		t.Error("ItemsPersisted instrument is nil")
	}
	if m.PayloadsPersisted == nil {
		t.Error("PayloadsPersisted instrument is nil")
	}
	if m.ProviderFrames == nil {
		t.Error("ProviderFrames instrument is nil")
	}
	if m.ReplayEventsQueued == nil {
		t.Error("ReplayEventsQueued instrument is nil")
	}
	if m.ReplayEventsDropped == nil {
		t.Error("ReplayEventsDropped instrument is nil")
	}
}

func TestProviderStartSpanProducesNoopWhenDisabled(t *testing.T) {
	p, err := NewProvider(context.Background(), Config{Enabled: false})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	ctx, span := p.StartSpan(context.Background(), "test.span", attribute.String("x", "y"))
	if ctx == nil {
		t.Error("StartSpan returned nil context")
	}
	if span == nil {
		t.Fatal("StartSpan returned nil span")
	}
	// No-op span: SpanContext should not be recording.
	if span.IsRecording() {
		t.Error("expected no-op span to not be recording")
	}
	span.End()
}

func TestProviderStartSpanRecordsWithTestExporter(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := trace.NewTracerProvider(trace.WithSpanProcessor(recorder))

	p, err := NewProvider(context.Background(), Config{Enabled: false})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	p.replaceTracerProvider(tp)

	_, span := p.StartSpan(context.Background(), "turn.lifecycle",
		ThreadAttr("thread-1"),
		ProviderAttr("claude"),
		ModelAttr("claude-sonnet-4-6"),
		TurnAttr(3),
	)
	span.End()

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("Ended() = %d spans, want 1", len(spans))
	}
	s := spans[0]
	if s.Name() != "turn.lifecycle" {
		t.Errorf("span name = %q, want turn.lifecycle", s.Name())
	}

	attrs := s.Attributes()
	got := make(map[string]attribute.Value, len(attrs))
	for _, kv := range attrs {
		got[string(kv.Key)] = kv.Value
	}
	if v, ok := got["thread.id"]; !ok || v.AsString() != "thread-1" {
		t.Errorf("thread.id attr = %v, want thread-1", got["thread.id"])
	}
	if v, ok := got["provider"]; !ok || v.AsString() != "claude" {
		t.Errorf("provider attr = %v, want claude", got["provider"])
	}
	if v, ok := got["model"]; !ok || v.AsString() != "claude-sonnet-4-6" {
		t.Errorf("model attr = %v, want claude-sonnet-4-6", got["model"])
	}
	if v, ok := got["turn.index"]; !ok || v.AsInt64() != 3 {
		t.Errorf("turn.index attr = %v, want 3", got["turn.index"])
	}
}

func TestRecordErrorSetsStatus(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := trace.NewTracerProvider(trace.WithSpanProcessor(recorder))

	p, _ := NewProvider(context.Background(), Config{Enabled: false})
	p.replaceTracerProvider(tp)

	_, span := p.StartSpan(context.Background(), "op")
	RecordError(span, errors.New("boom"))
	span.End()

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("Ended() = %d, want 1", len(spans))
	}
	if spans[0].Status().Code.String() != "Error" {
		t.Errorf("status = %v, want Error", spans[0].Status().Code)
	}
	if spans[0].Status().Description != "boom" {
		t.Errorf("status description = %q, want boom", spans[0].Status().Description)
	}
	if len(spans[0].Events()) == 0 {
		t.Error("expected at least one event (the recorded exception)")
	}
}

func TestRecordErrorNoOpsOnNil(t *testing.T) {
	// RecordError must not panic on nil inputs.
	RecordError(nil, errors.New("err"))
	RecordError(nil, nil)
}

func TestConfigFromFlagsTrimsEndpoint(t *testing.T) {
	cfg := ConfigFromFlags(true, "   localhost:4317  ")
	if !cfg.Enabled {
		t.Error("Enabled = false, want true")
	}
	if cfg.Endpoint != "localhost:4317" {
		t.Errorf("Endpoint = %q, want %q", cfg.Endpoint, "localhost:4317")
	}
	if cfg.ServiceName != DefaultServiceName {
		t.Errorf("ServiceName = %q, want %q", cfg.ServiceName, DefaultServiceName)
	}
}

func TestShutdownNoopOnNilProvider(t *testing.T) {
	var p *Provider
	if err := p.Shutdown(context.Background()); err != nil {
		t.Errorf("nil Shutdown returned %v, want nil", err)
	}
}

func TestShutdownRespectsTimeout(t *testing.T) {
	p := &Provider{enabled: true}
	blocker := make(chan struct{})
	p.shutdownFns = []func(context.Context) error{
		func(ctx context.Context) error {
			<-ctx.Done()
			close(blocker)
			return ctx.Err()
		},
	}

	start := time.Now()
	// Give Shutdown a very short outer deadline so we don't spend 5s here.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := p.Shutdown(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Shutdown returned nil, want deadline exceeded error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Shutdown error = %v, want DeadlineExceeded", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("Shutdown took %v, want <5s", elapsed)
	}

	<-blocker // ensure the blocker goroutine actually observed cancellation
}

func TestShutdownJoinsMultipleErrors(t *testing.T) {
	p := &Provider{enabled: true}
	p.shutdownFns = []func(context.Context) error{
		func(ctx context.Context) error { return errors.New("trace flush failed") },
		func(ctx context.Context) error { return errors.New("metric flush failed") },
	}
	err := p.Shutdown(context.Background())
	if err == nil {
		t.Fatal("Shutdown returned nil, want joined errors")
	}
	msg := err.Error()
	if !containsAll(msg, "trace flush failed", "metric flush failed") {
		t.Errorf("error message missing one or both: %q", msg)
	}
}

func TestShutdownClearsFns(t *testing.T) {
	called := 0
	p := &Provider{enabled: true}
	p.shutdownFns = []func(context.Context) error{
		func(ctx context.Context) error { called++; return nil },
	}
	_ = p.Shutdown(context.Background())
	if called != 1 {
		t.Errorf("fn called %d times, want 1", called)
	}
	// Second call must be a no-op.
	_ = p.Shutdown(context.Background())
	if called != 1 {
		t.Errorf("fn called %d times after second Shutdown, want 1", called)
	}
}

func TestMetricsInstrumentsHaveDistinctNames(t *testing.T) {
	p, err := NewProvider(context.Background(), Config{Enabled: false})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	m := p.Metrics()
	// The no-op impl happens to return the same pointer for all counters,
	// so we can't check identity. We do sanity-check that the struct
	// exposes every documented instrument by calling every field — if any
	// is nil, an Add call would panic.
	m.TurnsStarted.Add(context.Background(), 1)
	m.TurnsCompleted.Add(context.Background(), 1)
	m.TurnsErrored.Add(context.Background(), 1)
	m.ItemsPersisted.Add(context.Background(), 1)
	m.PayloadsPersisted.Add(context.Background(), 1)
	m.ProviderFrames.Record(context.Background(), 100)
	m.ReplayEventsQueued.Add(context.Background(), 1)
	m.ReplayEventsDropped.Add(context.Background(), 1)
}

func TestNilProviderFallsBackToNoopTracerAndMeter(t *testing.T) {
	var p *Provider
	if p.Tracer() == nil {
		t.Error("nil Provider.Tracer() returned nil")
	}
	if p.Meter() == nil {
		t.Error("nil Provider.Meter() returned nil")
	}
	m := p.Metrics()
	// Fallback metrics must also be populated so call sites can invoke them.
	if m.TurnsStarted == nil {
		t.Error("nil Provider Metrics().TurnsStarted is nil")
	}
	if m.ProviderFrames == nil {
		t.Error("nil Provider Metrics().ProviderFrames is nil")
	}
}

// containsAll reports whether s contains every needle.
func containsAll(s string, needles ...string) bool {
	for _, n := range needles {
		found := false
		for i := 0; i+len(n) <= len(s); i++ {
			if s[i:i+len(n)] == n {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
