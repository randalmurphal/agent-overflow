// Package otel wires OpenTelemetry OTLP tracing and metrics for agent-overflow.
//
// The package exposes a single entry point, Provider, that owns tracer and
// meter providers configured from user settings. The provider is created at
// ServiceStartup and shut down at ServiceShutdown. It never uses otel's
// package-global tracer/meter — callers receive the Provider and pass spans
// explicitly via StartSpan.
//
// Telemetry is opt-in. When the user has not enabled tracing, the provider
// returns no-op tracer/meter implementations; instrumented call sites pay
// nothing more than a single interface dispatch.
package otel
