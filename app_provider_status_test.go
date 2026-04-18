package main

import (
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
)

// collectProviderStatusEmissions wires a testEmitHook onto the given App and
// returns a slice that captures every `provider:status` emission. The
// returned slice is populated in-place as new events come in, so callers
// read it after driving the code under test.
func collectProviderStatusEmissions(a *App) *[]ProviderStatusEvent {
	events := &[]ProviderStatusEvent{}
	a.testEmitHook = func(name string, data any) {
		if name != "provider:status" {
			return
		}
		env, ok := data.(SeqEnvelope)
		if !ok {
			return
		}
		evt, ok := env.Data.(ProviderStatusEvent)
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
// through a.emit so the seq envelope is stamped automatically.
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

	var claudeEvent *ProviderStatusEvent
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

// TestProviderStatusEventFromDetectMapsVersionTooOld covers the Codex
// case: when DetectProvider returns Status="version_too_old" we pass
// through the message (which comes from formatCodexCLIUpgradeMessage)
// and mark the event actionable. Kept as a unit test on the mapping
// helper so the test doesn't depend on spawning a binary that emits a
// stale version.
func TestProviderStatusEventFromDetectMapsVersionTooOld(t *testing.T) {
	in := provider.ProviderStatus{
		Provider:   string(provider.Codex),
		Installed:  true,
		Version:    "codex 0.36.0",
		BinaryPath: "/usr/local/bin/codex",
		Status:     "version_too_old",
		Message:    "Codex CLI v0.36.0 is too old for Agent Overflow. Upgrade to v0.37.0 or newer and restart the app.",
	}
	got := providerStatusEventFromDetect(in)
	if got.Status != "version_too_old" {
		t.Fatalf("Status = %q, want version_too_old", got.Status)
	}
	if got.Message != in.Message {
		t.Fatalf("Message = %q, want %q (passthrough)", got.Message, in.Message)
	}
	if got.Version != in.Version {
		t.Fatalf("Version = %q, want %q", got.Version, in.Version)
	}
	if !got.Actionable {
		t.Fatal("version_too_old must be actionable")
	}
	// version_too_old does NOT carry an ActionURL — the remediation is
	// "run your package manager + restart the app", which isn't a link.
	if got.ActionURL != "" {
		t.Fatalf("ActionURL = %q, want empty for version_too_old", got.ActionURL)
	}
}

// TestProviderStatusEventFromDetectSkipsActionForReady covers the
// "ready -> no banner" branch explicitly so a future refactor can't
// accidentally start populating Actionable/Message on the happy path.
func TestProviderStatusEventFromDetectSkipsActionForReady(t *testing.T) {
	in := provider.ProviderStatus{
		Provider: string(provider.Claude),
		Status:   "ready",
		Version:  "claude-code 2.0.0",
	}
	got := providerStatusEventFromDetect(in)
	if got.Status != "ready" {
		t.Fatalf("Status = %q, want ready", got.Status)
	}
	if got.Actionable {
		t.Fatal("ready must not be actionable — banner should be empty")
	}
	if got.Message != "" {
		t.Fatalf("Message = %q, want empty for ready", got.Message)
	}
	if got.ActionURL != "" {
		t.Fatalf("ActionURL = %q, want empty for ready", got.ActionURL)
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

// TestClaudeUnauthenticatedStatusDetection sanity-checks the helper
// that decides whether an AccountInfo counts as unauthenticated. The
// rule mirrors forge's subscription-probe path: empty subscription
// AND empty token source is treated as "logged out".
func TestClaudeUnauthenticatedStatusDetection(t *testing.T) {
	cases := []struct {
		name string
		info provider.AccountInfo
		want bool
	}{
		{"empty account", provider.AccountInfo{}, true},
		{"has subscription", provider.AccountInfo{SubscriptionType: "pro"}, false},
		{"has token source", provider.AccountInfo{TokenSource: "oauth"}, false},
		{"has both", provider.AccountInfo{SubscriptionType: "max", TokenSource: "oauth"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeUnauthenticatedStatus(tc.info); got != tc.want {
				t.Fatalf("claudeUnauthenticatedStatus(%+v) = %v, want %v", tc.info, got, tc.want)
			}
		})
	}
}

// TestProviderActionURLTable guards the Go-owned URL table against
// the frontend accidentally inventing its own. The table is small —
// when a new status is added here, the component switch needs to
// pick it up, and vice versa.
func TestProviderActionURLTable(t *testing.T) {
	cases := []struct {
		provider string
		status   string
		wantSet  bool
	}{
		{string(provider.Claude), "not_found", true},
		{string(provider.Claude), "unauthenticated", true},
		{string(provider.Codex), "not_found", true},
		{string(provider.Codex), "version_too_old", false}, // "upgrade + restart", no single URL
		{string(provider.Claude), "ready", false},
		{string(provider.Claude), "error", false},
	}
	for _, tc := range cases {
		got := providerActionURL(tc.provider, tc.status)
		if tc.wantSet && got == "" {
			t.Fatalf("providerActionURL(%q, %q) = empty, want a URL", tc.provider, tc.status)
		}
		if !tc.wantSet && got != "" {
			t.Fatalf("providerActionURL(%q, %q) = %q, want empty", tc.provider, tc.status, got)
		}
	}
}
