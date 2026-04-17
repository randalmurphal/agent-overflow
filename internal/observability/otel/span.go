package otel

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// StartSpan is a one-liner convenience for the common case of "open a span,
// attach these attributes, remember to close it." It returns a derived
// context and the span; callers are expected to defer span.End().
//
// When p is nil or disabled, the returned span is a no-op. Attribute keys
// are still evaluated so the call site remains the same — that's cheap but
// worth noting if a caller finds itself building an expensive value purely
// for an attribute.
func (p *Provider) StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	tracer := p.Tracer()
	opts := []trace.SpanStartOption{}
	if len(attrs) > 0 {
		opts = append(opts, trace.WithAttributes(attrs...))
	}
	return tracer.Start(ctx, name, opts...)
}

// RecordError marks a span as errored and attaches the error's message. Safe
// on a no-op span (the call becomes a cheap interface dispatch).
func RecordError(span trace.Span, err error) {
	if err == nil || span == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

// ThreadAttr produces a canonical attribute for tagging spans with the
// thread id. Kept separate from the call site so we don't scatter the
// attribute key literal across the codebase.
func ThreadAttr(threadID string) attribute.KeyValue {
	return attribute.String("thread.id", threadID)
}

// ProviderAttr tags spans with the provider name (claude or codex).
func ProviderAttr(name string) attribute.KeyValue {
	return attribute.String("provider", name)
}

// ModelAttr tags spans with the model slug used for the turn.
func ModelAttr(model string) attribute.KeyValue {
	return attribute.String("model", model)
}

// TurnAttr tags spans with the turn index within a thread.
func TurnAttr(turnIndex int) attribute.KeyValue {
	return attribute.Int("turn.index", turnIndex)
}
