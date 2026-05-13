package main

import (
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/providerstatus"
	"agent-overflow/internal/settings"
)

// collectProviderStatusEmissions wires a testEmitHook onto the given App and
// returns a slice that captures every `provider:status` emission. The
// returned slice is populated in-place as new events come in, so callers
// read it after driving the code under test.
func collectProviderStatusEmissions(a *App) *[]providerstatus.Event {
	events := &[]providerstatus.Event{}
	a.testEmitHook = func(name string, data any) {
		if name != "provider:status" {
			return
		}
		evt, ok := data.(providerstatus.Event)
		if !ok {
			return
		}
		*events = append(*events, evt)
	}
	return events
}

// TestGetProviderStatusesEmitsNotFoundForMissingBinary is the headline
// case: a Claude binary that doesn't exist produces a provider:status
// event with Status="not_found" and a non-empty Message. The emit flows
// through a.emit so the transport bus stamps its per-channel seq before
// the wire frame goes out — the test observes the raw payload via the
// testEmitHook seam.
func TestGetProviderStatusesEmitsNotFoundForMissingBinary(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	if _, err := app.settings.Update(map[string]any{
		"claudeBinaryPath": "/nonexistent/claude-missing-binary-xyz",
		"codexBinaryPath":  "/nonexistent/codex-missing-binary-xyz",
	}); err != nil {
		t.Fatalf("settings.Update: %v", err)
	}
	events := collectProviderStatusEmissions(app)

	statuses, err := app.GetProviderStatuses()
	if err != nil {
		t.Fatalf("GetProviderStatuses: %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("want 2 statuses, got %d", len(statuses))
	}

	// Both providers should emit — neither resolves to a real binary.
	if len(*events) != 2 {
		t.Fatalf("want 2 provider:status emissions, got %d", len(*events))
	}

	var claudeEvent *providerstatus.Event
	for i, ev := range *events {
		if ev.Provider == string(provider.Claude) {
			claudeEvent = &(*events)[i]
		}
	}
	if claudeEvent == nil {
		t.Fatalf("no claude event in emissions: %+v", *events)
	}
	if claudeEvent.Status != "not_found" {
		t.Fatalf("claude status = %q, want not_found", claudeEvent.Status)
	}
	if claudeEvent.Message == "" {
		t.Fatal("claude message must be non-empty for not_found")
	}
	if !claudeEvent.Actionable {
		t.Fatal("not_found must be actionable so the UI renders a button")
	}
	if claudeEvent.ActionURL == "" {
		t.Fatal("not_found must carry an ActionURL (Go owns the URL table)")
	}
}

// TestGetProviderStatusesDoesNotEmitForReadyProvider confirms the
// happy-path short-circuit: a binary that resolves cleanly doesn't
// fire a provider:status event. Otherwise the UI would toggle the
// banner on every poll from settings, causing flicker.
func TestGetProviderStatusesDoesNotEmitForReadyProvider(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	// "echo" is always on PATH; DetectProvider returns Status="ready"
	// because --version completes without error, even though the output
	// doesn't parse as a real version.
	if _, err := app.settings.Update(map[string]any{
		"claudeBinaryPath": "echo",
		"codexBinaryPath":  "echo",
	}); err != nil {
		t.Fatalf("settings.Update: %v", err)
	}
	events := collectProviderStatusEmissions(app)

	if _, err := app.GetProviderStatuses(); err != nil {
		t.Fatalf("GetProviderStatuses: %v", err)
	}
	if len(*events) != 0 {
		t.Fatalf("want 0 emissions for ready providers, got %d: %+v", len(*events), *events)
	}
}

// TestEmitProviderStatusesFromDetectIsIdempotent covers the
// "re-emit same state is harmless" contract called out in the task.
// A duplicated DetectProvider result produces two identical events;
// the code under test must not crash, deduplicate at the Go layer, or
// mutate state that changes between calls.
func TestEmitProviderStatusesFromDetectIsIdempotent(t *testing.T) {
	app := newTestAppWithStore(t)
	events := collectProviderStatusEmissions(app)

	statuses := []provider.ProviderStatus{
		{
			Provider:   string(provider.Codex),
			Installed:  true,
			Status:     "version_too_old",
			Version:    "codex 0.36.0",
			BinaryPath: "/usr/local/bin/codex",
			Message:    "Codex CLI v0.36.0 is too old for Agent Overflow.",
		},
	}
	app.emitProviderStatusesFromDetect(statuses)
	app.emitProviderStatusesFromDetect(statuses)

	if len(*events) != 2 {
		t.Fatalf("want 2 emissions (idempotent re-emit), got %d", len(*events))
	}
	for i, ev := range *events {
		if ev.Status != "version_too_old" {
			t.Fatalf("emission[%d].Status = %q, want version_too_old", i, ev.Status)
		}
		if ev.Provider != string(provider.Codex) {
			t.Fatalf("emission[%d].Provider = %q, want codex", i, ev.Provider)
		}
	}
}

// TestEmitClaudeUnauthenticatedStatusShape guarantees the Claude
// unauthenticated emission carries the fields the UI branches on.
// Expressed as a direct call on the emit helper so the test doesn't
// need a running Claude probe.
func TestEmitClaudeUnauthenticatedStatusShape(t *testing.T) {
	app := newTestAppWithStore(t)
	events := collectProviderStatusEmissions(app)

	app.emitClaudeUnauthenticatedStatus()

	if len(*events) != 1 {
		t.Fatalf("want 1 emission, got %d", len(*events))
	}
	evt := (*events)[0]
	if evt.Provider != string(provider.Claude) {
		t.Fatalf("Provider = %q, want claude", evt.Provider)
	}
	if evt.Status != "unauthenticated" {
		t.Fatalf("Status = %q, want unauthenticated", evt.Status)
	}
	if evt.Message == "" {
		t.Fatal("Message must be populated for unauthenticated")
	}
	if !evt.Actionable {
		t.Fatal("unauthenticated must be actionable")
	}
}
