package main

import (
	"encoding/json"
	"reflect"
	"strings"

	obsotel "agent-overflow/internal/observability/otel"
	"agent-overflow/internal/observability/replay"
	"agent-overflow/internal/settings"
)

// Telemetry returns the active OpenTelemetry provider. Returns nil when the
// app has not completed ServiceStartup. Exported for tests that need to
// inspect or replace the provider at runtime.
func (a *App) Telemetry() *obsotel.Provider {
	return a.telemetry
}

// ReplayManager returns the active replay manager. May be nil before startup.
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

// threadIDFromEvent does a best-effort lookup of the thread id on an event
// payload. Wails emit takes `any`, so the payload can be almost anything;
// we handle the common shapes rather than trying to enumerate every event
// struct in the codebase.
//
// The lookup order is:
//  1. map[string]any / map[string]string with a "threadId" key (the most
//     common shape for frontend-visible events).
//  2. Struct with an exported ThreadID field (our ProviderEvent shape).
//  3. JSON round-trip fallback for anonymous struct literals that embed
//     a `threadId` tag without exposing a direct Go field name we can
//     reach via reflection.
//
// Returns the empty string if no thread id is found.
func threadIDFromEvent(data any) string {
	if data == nil {
		return ""
	}

	switch payload := data.(type) {
	case map[string]any:
		if id, ok := payload["threadId"].(string); ok {
			return strings.TrimSpace(id)
		}
	case map[string]string:
		return strings.TrimSpace(payload["threadId"])
	case string:
		return ""
	}

	v := reflect.ValueOf(data)
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return ""
		}
		v = v.Elem()
	}
	if v.Kind() == reflect.Struct {
		if f := v.FieldByName("ThreadID"); f.IsValid() && f.Kind() == reflect.String {
			return strings.TrimSpace(f.String())
		}
	}

	// Fallback: marshal through JSON and look for a "threadId" top-level
	// field. Costs an allocation; only hit when the structured lookups
	// above miss, which is rare in practice.
	encoded, err := json.Marshal(data)
	if err != nil {
		return ""
	}
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &generic); err != nil {
		return ""
	}
	raw, ok := generic["threadId"]
	if !ok {
		return ""
	}
	var id string
	if err := json.Unmarshal(raw, &id); err != nil {
		return ""
	}
	return strings.TrimSpace(id)
}

