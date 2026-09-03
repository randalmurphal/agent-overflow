package app

import (
	"time"

	obsotel "agent-overflow/internal/observability/otel"
	"agent-overflow/internal/observability/replay"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/uitrace"
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
//
//ao:scope host
//ao:route home
func (a *App) ReconfigureObservability(prev, next settings.Settings) {
	if a.replay != nil {
		a.replay.SetEnabled(next.ObservabilityEventLogEnabled)
	}
	_ = prev // reserved for future diff-aware logic (e.g. surfacing a "restart required" banner)
}

// uiTrace returns the lazy-initialized Tracer keyed on a.configDir.
// Construction is one-shot so subsequent calls reuse the same Tracer
// (and its appendMutex). Returning the same error on every retry — see
// uitrace.New — means a misconfigured App (empty configDir) fails loudly
// on every binding call instead of silently no-op'ing after the first.
func (a *App) uiTrace() (*uitrace.Tracer, error) {
	a.uiTraceOnce.Do(func() {
		a.uiTracer, a.uiTraceErr = uitrace.New(a.configDir)
	})
	return a.uiTracer, a.uiTraceErr
}

// GetUIRenderTracePath returns the dev trace file path used by
// AppendUIRenderTraceBatch. The frontend exposes it through the console trace
// API so a debug run can be inspected after a visual glitch.
//
//ao:scope host
//ao:route home
func (a *App) GetUIRenderTracePath() (string, error) {
	t, err := a.uiTrace()
	if err != nil {
		return "", err
	}
	return t.Path(), nil
}

// AppendUIRenderTraceBatch appends compact dev-only UI render trace records.
// The frontend batches calls so rendering never waits on disk. The binding
// validates each line because it writes directly into the user's config
// directory.
//
//ao:scope host
//ao:route home
func (a *App) AppendUIRenderTraceBatch(lines []string) (string, error) {
	t, err := a.uiTrace()
	if err != nil {
		return "", err
	}
	return t.Append(lines)
}

// frontendErrorLog returns the lazy-initialized always-on frontend error
// appender, mirroring uiTrace's construction contract.
func (a *App) frontendErrorLog() (*uitrace.Tracer, error) {
	a.frontendErrorsOnce.Do(func() {
		a.frontendErrors, a.frontendErrorsErr = uitrace.NewErrors(a.configDir)
	})
	return a.frontendErrors, a.frontendErrorsErr
}

// ReportFrontendErrorBatch appends frontend runtime-error records (window
// `error` / `unhandledrejection` events captured by the global handlers in
// frontend/src/lib/utils/frontendErrorCapture.ts) to
// <configDir>/ui-trace/frontend-errors.jsonl. Unlike the render trace this
// channel is always on: a render-path exception is user-facing state we
// must be able to diagnose after the fact, and silent frontend errors have
// already cost us a multi-day memory-leak hunt (every throw mid-update
// permanently leaks the deriveds that update had just connected).
//
//ao:scope host
//ao:route home
func (a *App) ReportFrontendErrorBatch(lines []string) (string, error) {
	t, err := a.frontendErrorLog()
	if err != nil {
		return "", err
	}
	return t.Append(lines)
}

// BookmarkUIRenderTrace freezes the current trace contents (and any
// rotated `.1` predecessor) into a non-rotating bookmark file under
// `<configDir>/ui-trace/bookmarks/`. The frontend invokes this from
// Ctrl+Shift+B so the bug-moment context survives the next rotation
// triggered by ongoing render activity. Returns the bookmark path
// (empty string if no trace data exists yet).
//
//ao:scope host
//ao:route home
func (a *App) BookmarkUIRenderTrace() (string, error) {
	t, err := a.uiTrace()
	if err != nil {
		return "", err
	}
	return t.Bookmark(time.Now())
}
