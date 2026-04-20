package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/design"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
)

// TestHandleClaudeDesignToolIgnoresNonToolStartEvents asserts that only
// EventToolStart events are considered. A text-delta event carrying a
// render_design item type must be ignored (no reactor call, no artifact).
func TestHandleClaudeDesignToolIgnoresNonToolStartEvents(t *testing.T) {
	app := newTestAppWithDesign(t)
	setThreadProvider(t, app, "thread-design", string(provider.Claude))

	calls := make(chan string, 4)
	app.reactor = design.NewReactor(app.artifacts, func(eventName string, _ any) {
		calls <- eventName
	})

	meta, err := json.Marshal(map[string]any{
		"toolName": "render_design",
		"input": map[string]any{
			"html":  "<html>ignored</html>",
			"title": "Ignored",
		},
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	app.handleClaudeDesignTool(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "thread-design",
		ItemType:  "render_design",
		Meta:      meta,
		Timestamp: time.Now(),
	})

	select {
	case got := <-calls:
		t.Fatalf("reactor emitted %q for non-tool-start event", got)
	case <-time.After(100 * time.Millisecond):
	}

	artifacts, err := app.ListDesignArtifacts("thread-design")
	if err != nil {
		t.Fatalf("ListDesignArtifacts() error = %v", err)
	}
	if len(artifacts) != 0 {
		t.Fatalf("len(artifacts) = %d, want 0", len(artifacts))
	}
}

// TestHandleClaudeDesignToolIgnoresUnrelatedToolNames confirms the switch is
// tight: a random tool (e.g. "Read") must not trigger the design reactor.
func TestHandleClaudeDesignToolIgnoresUnrelatedToolNames(t *testing.T) {
	app := newTestAppWithDesign(t)
	setThreadProvider(t, app, "thread-design", string(provider.Claude))

	emitted := make(chan string, 4)
	app.reactor = design.NewReactor(app.artifacts, func(eventName string, _ any) {
		emitted <- eventName
	})

	meta, err := json.Marshal(map[string]any{
		"toolName": "Read",
		"input":    map[string]any{"path": "README.md"},
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	app.handleClaudeDesignTool(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "thread-design",
		ItemType:  "Read",
		Meta:      meta,
		Timestamp: time.Now(),
	})

	select {
	case name := <-emitted:
		t.Fatalf("reactor emitted %q for unrelated tool", name)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestHandleClaudeDesignToolIgnoresCodexThread enforces the Claude-only gate
// — even if a Codex thread somehow surfaces a render_design tool call,
// handleClaudeDesignTool must skip it. Codex has its own MCP server path.
func TestHandleClaudeDesignToolIgnoresCodexThread(t *testing.T) {
	app := newTestAppWithDesign(t)
	setThreadProvider(t, app, "thread-design", string(provider.Codex))

	emitted := make(chan string, 4)
	app.reactor = design.NewReactor(app.artifacts, func(eventName string, _ any) {
		emitted <- eventName
	})

	meta, err := json.Marshal(map[string]any{
		"toolName": "render_design",
		"input": map[string]any{
			"html":  "<html>nope</html>",
			"title": "Nope",
		},
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	app.handleClaudeDesignTool(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "thread-design",
		ItemType:  "render_design",
		Meta:      meta,
		Timestamp: time.Now(),
	})

	select {
	case name := <-emitted:
		t.Fatalf("reactor emitted %q for Codex thread", name)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestHandleClaudeDesignToolIgnoresNonDesignInteractionMode makes sure that a
// Claude thread NOT in design mode can still make a render_design tool call
// without hitting the reactor. This prevents accidental design side effects
// for regular threads.
func TestHandleClaudeDesignToolIgnoresNonDesignInteractionMode(t *testing.T) {
	app := newTestAppWithStore(t)
	app.configDir = t.TempDir()
	app.artifacts = design.NewArtifactStore(filepath.Join(t.TempDir(), "design-artifacts"), app.store)
	emitted := make(chan string, 4)
	app.reactor = design.NewReactor(app.artifacts, func(eventName string, _ any) {
		emitted <- eventName
	})
	app.designMCP = codex.NewDesignMCPServer(app.reactor)
	t.Cleanup(func() { _ = app.designMCP.Close() })

	thread := testThread("thread-non-design")
	thread.Provider = string(provider.Claude)
	// mode stays "default" via testThread.
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	meta, err := json.Marshal(map[string]any{
		"toolName": "render_design",
		"input":    map[string]any{"html": "<html>x</html>", "title": "Nope"},
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	app.handleClaudeDesignTool(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  thread.ID,
		ItemType:  "render_design",
		Meta:      meta,
		Timestamp: time.Now(),
	})

	select {
	case name := <-emitted:
		t.Fatalf("reactor emitted %q for non-design thread", name)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestHandleClaudeDesignToolInvalidMetaEmitsErrorEvent: a garbage Meta
// payload should not panic — it must surface through the triage layer as an
// error event, which the UI renders as a tool failure.
func TestHandleClaudeDesignToolInvalidMetaEmitsErrorEvent(t *testing.T) {
	app := newTestAppWithDesign(t)
	setThreadProvider(t, app, "thread-design", string(provider.Claude))

	errorEvents := collectErrorItemUpserts(t, app, 4)

	app.handleClaudeDesignTool(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "thread-design",
		ItemType:  "render_design",
		Meta:      json.RawMessage("not json"),
		Timestamp: time.Now(),
	})

	select {
	case item := <-errorEvents:
		if !strings.Contains(item.Summary, "render_design") {
			t.Fatalf("error content = %q, want render_design context", item.Summary)
		}
		if !strings.Contains(item.Summary, "invalid input") {
			t.Fatalf("error content = %q, want invalid input phrasing", item.Summary)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for error item")
	}
}

// TestHandleClaudeDesignToolMissingInputEmitsErrorEvent: well-formed outer
// envelope but no "input" key. The check on meta.Input length must fire.
func TestHandleClaudeDesignToolMissingInputEmitsErrorEvent(t *testing.T) {
	app := newTestAppWithDesign(t)
	setThreadProvider(t, app, "thread-design", string(provider.Claude))

	errorEvents := collectErrorItemUpserts(t, app, 4)

	// outer envelope has toolName but no input.
	meta, err := json.Marshal(map[string]any{"toolName": "present_options"})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	app.handleClaudeDesignTool(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "thread-design",
		ItemType:  "present_options",
		Meta:      meta,
		Timestamp: time.Now(),
	})

	select {
	case item := <-errorEvents:
		if !strings.Contains(item.Summary, "present_options") {
			t.Fatalf("error content = %q, want present_options context", item.Summary)
		}
		if !strings.Contains(item.Summary, "missing input") {
			t.Fatalf("error content = %q, want missing input phrasing", item.Summary)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for missing-input error item")
	}
}

// TestHandleClaudeDesignToolRenderInvalidInputEmitsError exercises the inner
// JSON unmarshal error branch: the outer envelope is fine, but the
// render_design input is garbage.
func TestHandleClaudeDesignToolRenderInvalidInputEmitsError(t *testing.T) {
	app := newTestAppWithDesign(t)
	setThreadProvider(t, app, "thread-design", string(provider.Claude))

	errorEvents := collectErrorItemUpserts(t, app, 4)

	// input is a JSON number, which cannot unmarshal into RenderInput.
	meta := json.RawMessage(`{"toolName":"render_design","input":42}`)

	app.handleClaudeDesignTool(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "thread-design",
		ItemType:  "render_design",
		Meta:      meta,
		Timestamp: time.Now(),
	})

	select {
	case item := <-errorEvents:
		if !strings.Contains(item.Summary, "render_design") {
			t.Fatalf("error content = %q, want render_design context", item.Summary)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for render input error item")
	}
}

// TestHandleClaudeDesignToolTeardownCancelsPendingOptions asserts that when
// teardownDesignThread runs (e.g. StopSession), a pending present_options
// call resolves with ErrDesignSessionEnded and the handler does NOT emit
// an error event (the error is the expected cancellation signal). It also
// ensures no follow-up sendMessage is produced.
func TestHandleClaudeDesignToolTeardownCancelsPendingOptions(t *testing.T) {
	app, presented := newTestAppWithDesignNotify(t)
	setThreadProvider(t, app, "thread-design", string(provider.Claude))

	errorEvents := collectErrorItemUpserts(t, app, 4)

	sent := make(chan string, 1)
	app.sendMessageFn = func(threadID, content string) error {
		sent <- content
		return nil
	}

	meta, err := json.Marshal(map[string]any{
		"toolName": "present_options",
		"input": map[string]any{
			"prompt": "Choose",
			"options": []map[string]any{
				{"id": "a", "title": "Alpha", "description": "", "html": "<html>A</html>"},
				{"id": "b", "title": "Beta", "description": "", "html": "<html>B</html>"},
			},
		},
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	app.handleClaudeDesignTool(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "thread-design",
		ItemType:  "present_options",
		Meta:      meta,
		Timestamp: time.Now(),
	})

	// Wait for the reactor to register the pending request before tearing
	// it down — otherwise we might race the goroutine.
	waitForAppDesignRequest(t, app, presented)

	// Simulate session stop / teardown.
	app.teardownDesignThread("thread-design")

	// No follow-up sendMessage should fire — the user never chose.
	select {
	case content := <-sent:
		t.Fatalf("unexpected follow-up sendMessage after teardown: %q", content)
	case <-time.After(150 * time.Millisecond):
	}

	// No error should surface either (ErrDesignSessionEnded is suppressed).
	select {
	case item := <-errorEvents:
		t.Fatalf("unexpected error item after teardown: %q", item.Summary)
	case <-time.After(150 * time.Millisecond):
	}
}

// TestHandleClaudeDesignToolOptionsInvalidInputEmitsError covers the inner
// JSON unmarshal branch for present_options.
func TestHandleClaudeDesignToolOptionsInvalidInputEmitsError(t *testing.T) {
	app := newTestAppWithDesign(t)
	setThreadProvider(t, app, "thread-design", string(provider.Claude))

	errorEvents := collectErrorItemUpserts(t, app, 4)

	// input is an array, which fails PresentOptionsInput unmarshal.
	meta := json.RawMessage(`{"toolName":"present_options","input":[1,2,3]}`)

	app.handleClaudeDesignTool(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "thread-design",
		ItemType:  "present_options",
		Meta:      meta,
		Timestamp: time.Now(),
	})

	select {
	case item := <-errorEvents:
		if !strings.Contains(item.Summary, "present_options") {
			t.Fatalf("error content = %q, want present_options context", item.Summary)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for options input error item")
	}
}

// TestFormatClaudeDesignChoiceIncludesTitle verifies the follow-up message
// format fed back to Claude after the user selects a design option. This is
// the contract boundary between the design reactor and the Claude prompt.
func TestFormatClaudeDesignChoiceIncludesTitle(t *testing.T) {
	got := formatClaudeDesignChoice(design.ChoiceResult{Chosen: "b", Title: "Beta"})
	if !strings.Contains(got, `"Beta"`) {
		t.Fatalf("format = %q, want title in quotes", got)
	}
	if !strings.Contains(got, "ID: b") {
		t.Fatalf("format = %q, want option ID", got)
	}
}

// TestFormatClaudeDesignChoiceFallsBackToIDWithoutTitle ensures empty-title
// selections still produce a valid follow-up message using the option ID.
func TestFormatClaudeDesignChoiceFallsBackToIDWithoutTitle(t *testing.T) {
	got := formatClaudeDesignChoice(design.ChoiceResult{Chosen: "c"})
	if strings.Contains(got, `""`) {
		t.Fatalf("format = %q, empty quoted title leaked through", got)
	}
	if !strings.Contains(got, "option c") {
		t.Fatalf("format = %q, want ID-based fallback", got)
	}
}
