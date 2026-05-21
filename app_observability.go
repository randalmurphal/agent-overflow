package main

import (
	obsotel "agent-overflow/internal/observability/otel"
	"agent-overflow/internal/observability/replay"
	"agent-overflow/internal/settings"
)

// Telemetry returns the active OpenTelemetry provider. Returns nil when the
// app has not completed ServiceStartup. Exported for tests that need to
// inspect or replace the provider at runtime.
//
// `//wails:ignore` keeps this off the wire — `*obsotel.Provider` is a
// process-internal handle, not a JSON-serialisable value, and nothing in
// the frontend should reach in for it.
//
//wails:ignore
func (a *App) Telemetry() *obsotel.Provider {
	return a.telemetry
}

// ReplayManager returns the active replay manager. May be nil before startup.
//
// `//wails:ignore` for the same reason as Telemetry — internal handle,
// not a wire-exposed value.
//
//wails:ignore
func (a *App) ReplayManager() *replay.Manager {
	return a.replay
}

// ReconfigureObservability reconciles the live observability stack with the
// current Settings snapshot. Tracing changes always require restart (we
// don't hot-swap tracer providers — that path is too easy to break in
// ways the user only sees when a trace goes missing). The replay toggle
// is cheap to flip in place.
//
// This is the hook the UI calls after UpdateSettings persists a change —
// see app_settings.go.
func (a *App) ReconfigureObservability(prev, next settings.Settings) {
	if a.replay != nil {
		a.replay.SetEnabled(next.ObservabilityEventLogEnabled)
	}
	_ = prev // reserved for future diff-aware logic (e.g. surfacing a "restart required" banner)
}
