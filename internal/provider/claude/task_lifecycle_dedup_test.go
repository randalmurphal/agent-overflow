package claude

import (
	"testing"

	"agent-overflow/internal/provider"
)

// TestTaskNotificationDedupsAfterTerminalTaskUpdated covers the ordering
// guarantee for Claude's background-task lifecycle: the terminal
// task_updated event produces exactly one EventToolComplete, and a
// later task_notification carrying the same taskID must NOT produce a
// second completion.
//
// Without this dedup the background tool card would double-settle:
// first via task_updated (the real lifecycle edge), then again via the
// informational task_notification that follows. Duplicate
// EventToolComplete would cause the router's background-done path to
// append a second tool_completion row for the same launch, which breaks
// the two-row contract documented in docs/architecture/data-flow.md.
//
// Distinct file name from any parallel Agent 2 additions (per Agent 4
// scope note); this test stands alone and doesn't share state.
func TestTaskNotificationDedupsAfterTerminalTaskUpdated(t *testing.T) {
	parser := NewParser()

	// 1. task_started primes the parser with the taskID → toolUseID map.
	startLine := []byte(`{"type":"system","subtype":"task_started","task_id":"task-dedup","tool_use_id":"tool-dedup"}`)
	if events, err := parser.ParseLine(testThread, startLine); err != nil {
		t.Fatalf("task_started: %v", err)
	} else if len(events) != 0 {
		t.Fatalf("task_started must not emit events, got %d", len(events))
	}

	// 2. Terminal task_updated is the ONLY source of truth for the
	// completion edge — it emits one EventToolComplete.
	updatedLine := []byte(`{"type":"system","subtype":"task_updated","task_id":"task-dedup","patch":{"status":"completed","description":"final"}}`)
	events, err := parser.ParseLine(testThread, updatedLine)
	if err != nil {
		t.Fatalf("task_updated: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("task_updated must emit exactly 1 completion, got %d", len(events))
	}
	if events[0].Kind != provider.EventToolComplete {
		t.Fatalf("task_updated event kind = %q, want %q", events[0].Kind, provider.EventToolComplete)
	}
	if events[0].ItemID != "tool-dedup" {
		t.Fatalf("task_updated itemID = %q, want tool-dedup", events[0].ItemID)
	}

	// 3. task_notification is informational only — it MUST NOT emit a
	// second completion even though it carries the same taskID and a
	// terminal status.
	notifLine := []byte(`{"type":"system","subtype":"task_notification","task_id":"task-dedup","tool_use_id":"tool-dedup","status":"completed","summary":"noise"}`)
	notifEvents, err := parser.ParseLine(testThread, notifLine)
	if err != nil {
		t.Fatalf("task_notification: %v", err)
	}
	if len(notifEvents) != 0 {
		t.Fatalf("task_notification must emit no events (dedup), got %d: %+v", len(notifEvents), notifEvents)
	}

	// 4. Defense-in-depth: a repeat task_updated for the same taskID
	// must also be suppressed. Providers occasionally replay terminal
	// edges (e.g. network retry on a progress poll) — the parser's
	// markTaskCompleted guards against this too.
	repeatEvents, err := parser.ParseLine(testThread, updatedLine)
	if err != nil {
		t.Fatalf("task_updated replay: %v", err)
	}
	if len(repeatEvents) != 0 {
		t.Fatalf("replayed task_updated must not re-emit, got %d", len(repeatEvents))
	}
}
