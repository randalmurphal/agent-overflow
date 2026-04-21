package claude

import (
	"testing"

	"agent-overflow/internal/provider"
)

// TestTaskLifecycleSignalsPostRefactor covers the ordering guarantee
// for Claude's background-task lifecycle after the parser refactor:
//
//  1. `task_started` emits a meta-only `EventToolStart` so triage can
//     persist the `task_id ↔ tool_use_id` mapping into `items.meta`.
//  2. Terminal `task_updated` emits exactly one
//     `EventBackgroundTaskTerminal` (NOT `EventToolComplete` — the
//     tool-lifecycle completion was already emitted by the
//     backgrounded placeholder's `tool_result`).
//  3. `task_notification` is informational only — it MUST NOT emit
//     any event (invariant 21).
//  4. A replayed `task_updated` re-emits (dedup is triage's
//     responsibility via idempotent AppendCompletionItem upsert);
//     the parser no longer carries dedup sets.
//
// Distinct file name from any parallel Agent 2 additions (per Agent 4
// scope note); this test stands alone and doesn't share state.
func TestTaskLifecycleSignalsPostRefactor(t *testing.T) {
	parser := NewParser()

	// 1. task_started primes the parser with the taskID → toolUseID map
	// AND emits a meta-only EventToolStart so triage persists the
	// task_id into items.meta for reconnect recovery.
	startLine := []byte(`{"type":"system","subtype":"task_started","task_id":"task-dedup","tool_use_id":"tool-dedup"}`)
	startEvents, err := parser.ParseLine(testThread, startLine)
	if err != nil {
		t.Fatalf("task_started: %v", err)
	}
	if len(startEvents) != 1 {
		t.Fatalf("task_started must emit 1 meta-carrying EventToolStart, got %d", len(startEvents))
	}
	if startEvents[0].Kind != provider.EventToolStart {
		t.Fatalf("task_started kind = %q, want %q", startEvents[0].Kind, provider.EventToolStart)
	}
	if startEvents[0].ItemID != "tool-dedup" {
		t.Fatalf("task_started ItemID = %q, want tool-dedup", startEvents[0].ItemID)
	}

	// 2. Terminal task_updated emits one EventBackgroundTaskTerminal.
	updatedLine := []byte(`{"type":"system","subtype":"task_updated","task_id":"task-dedup","patch":{"status":"completed","description":"final"}}`)
	events, err := parser.ParseLine(testThread, updatedLine)
	if err != nil {
		t.Fatalf("task_updated: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("task_updated must emit exactly 1 terminal, got %d", len(events))
	}
	if events[0].Kind != provider.EventBackgroundTaskTerminal {
		t.Fatalf("task_updated event kind = %q, want %q", events[0].Kind, provider.EventBackgroundTaskTerminal)
	}
	if events[0].ItemID != "tool-dedup" {
		t.Fatalf("task_updated itemID = %q, want tool-dedup", events[0].ItemID)
	}

	// 3. task_notification MUST emit zero events (invariant 21).
	notifLine := []byte(`{"type":"system","subtype":"task_notification","task_id":"task-dedup","tool_use_id":"tool-dedup","status":"completed","summary":"noise"}`)
	notifEvents, err := parser.ParseLine(testThread, notifLine)
	if err != nil {
		t.Fatalf("task_notification: %v", err)
	}
	if len(notifEvents) != 0 {
		t.Fatalf("task_notification must emit no events (invariant 21), got %d: %+v", len(notifEvents), notifEvents)
	}

	// 4. A replayed task_updated re-emits — the parser no longer
	// dedups. Triage's AppendCompletionItem is idempotent and handles
	// collision by upsert-replace on the stable completion id.
	repeatEvents, err := parser.ParseLine(testThread, updatedLine)
	if err != nil {
		t.Fatalf("task_updated replay: %v", err)
	}
	if len(repeatEvents) != 1 || repeatEvents[0].Kind != provider.EventBackgroundTaskTerminal {
		t.Fatalf("replayed task_updated must re-emit one EventBackgroundTaskTerminal (parser no longer dedups), got %+v", repeatEvents)
	}
}
