package claude

import (
	"testing"

	"agent-overflow/internal/provider"
)

// --- Gap 3: parent_tool_use_id subagent correlation ---
//
// Assistant messages that originate from a Task-tool child run carry a
// top-level `parent_tool_use_id` field that identifies the Task tool
// use. The parser preserves that field on every event it emits from
// the assistant envelope so downstream triage can group child turns
// with their parent Task item. (Text and thinking blocks are not
// emitted from the assistant envelope — they reach triage via
// stream_event partials, covered by the tests in
// `partial_messages_test.go`. The assistant envelope's text/thinking
// content is intentionally skipped to avoid doubling the summary.)

func TestParseAssistantCarriesParentToolUseIDOnToolStart(t *testing.T) {
	line := []byte(`{"type":"assistant","parent_tool_use_id":"task_tool_123","message":{"id":"msg-child","role":"assistant","content":[{"type":"tool_use","id":"inner-tool","name":"Bash","input":{"command":"ls"}}]}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	evt := events[0]
	if evt.Kind != provider.EventToolStart {
		t.Fatalf("kind: got %q, want %q", evt.Kind, provider.EventToolStart)
	}
	if evt.ParentToolUseID != "task_tool_123" {
		t.Errorf("parentToolUseID: got %q, want %q", evt.ParentToolUseID, "task_tool_123")
	}
}

func TestParseAssistantWithoutParentToolUseID(t *testing.T) {
	// Backward compat: top-level assistant messages (no
	// parent_tool_use_id) emit events with an empty ParentToolUseID.
	// Exercised via a tool_use block since text/thinking blocks are
	// intentionally skipped in parse_assistant — see the file-level
	// comment above.
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"Bash","input":{"command":"ls"}}]}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].ParentToolUseID != "" {
		t.Errorf("parentToolUseID: got %q, want empty", events[0].ParentToolUseID)
	}
}

func TestParseAssistantExitPlanModeCarriesParentToolUseID(t *testing.T) {
	// ExitPlanMode inside a subagent context should still propagate
	// the parent_tool_use_id onto the resulting proposed_plan event.
	line := []byte(`{"type":"assistant","parent_tool_use_id":"task_tool_xyz","message":{"id":"msg-plan","role":"assistant","content":[{"type":"tool_use","id":"tool-plan","name":"ExitPlanMode","input":{"plan":"# plan"}}]}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventProposedPlan {
		t.Fatalf("kind: got %q, want %q", events[0].Kind, provider.EventProposedPlan)
	}
	if events[0].ParentToolUseID != "task_tool_xyz" {
		t.Errorf("parentToolUseID: got %q, want %q",
			events[0].ParentToolUseID, "task_tool_xyz")
	}
}
