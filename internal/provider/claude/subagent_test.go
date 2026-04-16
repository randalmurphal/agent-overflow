package claude

import (
	"testing"

	"agent-overflow/internal/provider"
)

// --- Gap 3: parent_tool_use_id subagent correlation ---
//
// Assistant messages that originate from a Task-tool child run carry a
// top-level `parent_tool_use_id` field that identifies the Task tool use.
// The parser preserves that field on every event emitted for the
// assistant message so downstream triage can group child turns with
// their parent Task item.

func TestParseAssistantCarriesParentToolUseID(t *testing.T) {
	line := []byte(`{"type":"assistant","parent_tool_use_id":"task_tool_123","message":{"id":"msg-child","role":"assistant","content":[{"type":"text","text":"Child step"}]}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	evt := events[0]
	if evt.Kind != provider.EventTextDelta {
		t.Fatalf("kind: got %q, want %q", evt.Kind, provider.EventTextDelta)
	}
	if evt.ParentToolUseID != "task_tool_123" {
		t.Errorf("parentToolUseID: got %q, want %q", evt.ParentToolUseID, "task_tool_123")
	}
}

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

func TestParseAssistantCarriesParentToolUseIDOnThinking(t *testing.T) {
	line := []byte(`{"type":"assistant","parent_tool_use_id":"task_tool_123","message":{"id":"msg-child","role":"assistant","content":[{"type":"thinking","thinking":"sub-agent reasoning"}]}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventThinking {
		t.Fatalf("kind: got %q, want thinking", events[0].Kind)
	}
	if events[0].ParentToolUseID != "task_tool_123" {
		t.Errorf("parentToolUseID: got %q, want %q",
			events[0].ParentToolUseID, "task_tool_123")
	}
}

func TestParseAssistantWithoutParentToolUseID(t *testing.T) {
	// Backward compat: top-level assistant messages (no parent_tool_use_id)
	// emit events with an empty ParentToolUseID.
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":"top-level"}]}}`)

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
