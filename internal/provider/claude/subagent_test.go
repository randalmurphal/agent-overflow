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

func TestParseAssistantStampsSubagentModelOnceFromFirstSubagentEnvelope(t *testing.T) {
	// Subagent assistant envelopes carry the subagent's actual model id
	// in `message.model`. The parser turns the FIRST envelope per parent
	// into a meta-only EventToolStart targeting the parent tool_use_id
	// with `subagent_model` so triage merges it onto the parent
	// tool_call's items.meta. Subsequent envelopes for the same parent
	// must not re-emit (the model is stable for the subagent's
	// lifetime).
	parser := NewParser()
	first := []byte(`{"type":"assistant","parent_tool_use_id":"parent_tool_123","message":{"id":"msg-child-1","model":"claude-opus-4-7","role":"assistant","content":[{"type":"tool_use","id":"inner-tool","name":"Bash","input":{"command":"ls"}}]}}`)

	events, err := parser.ParseLine(testThread, first)
	if err != nil {
		t.Fatalf("parse first: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events (subagent_model meta + Bash tool start), got %d", len(events))
	}
	meta, tool := events[0], events[1]
	if meta.Kind != provider.EventToolStart {
		t.Fatalf("meta event kind: got %q, want %q", meta.Kind, provider.EventToolStart)
	}
	if meta.ItemID != "parent_tool_123" {
		t.Errorf("meta event item id: got %q, want parent tool_use id", meta.ItemID)
	}
	if got := string(meta.Meta); got != `{"subagent_model":"claude-opus-4-7"}` {
		t.Errorf("meta payload: got %s, want subagent_model=claude-opus-4-7", got)
	}
	if meta.ItemType != "" {
		t.Errorf("meta event must not carry an ItemType (toolName) — would clobber the existing row's tool_name; got %q", meta.ItemType)
	}
	if tool.Kind != provider.EventToolStart || tool.ItemID != "inner-tool" {
		t.Errorf("second event should be the inner Bash tool_use_id start; got kind=%q itemID=%q", tool.Kind, tool.ItemID)
	}

	// A second subagent envelope for the same parent (more text /
	// another tool) must NOT re-emit the subagent_model meta event.
	second := []byte(`{"type":"assistant","parent_tool_use_id":"parent_tool_123","message":{"id":"msg-child-2","model":"claude-opus-4-7","role":"assistant","content":[{"type":"tool_use","id":"inner-tool-2","name":"Bash","input":{"command":"pwd"}}]}}`)
	events, err = parser.ParseLine(testThread, second)
	if err != nil {
		t.Fatalf("parse second: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("second envelope should emit only the inner tool start, got %d events", len(events))
	}
	if events[0].ItemID != "inner-tool-2" {
		t.Errorf("expected the inner Bash tool start; got %q", events[0].ItemID)
	}

	// A different parent with its own subagent_model SHOULD emit a meta
	// update (per-parent dedupe, not global).
	other := []byte(`{"type":"assistant","parent_tool_use_id":"parent_tool_other","message":{"id":"msg-other","model":"claude-haiku-4-5-20251001","role":"assistant","content":[{"type":"tool_use","id":"inner-other","name":"Bash","input":{"command":"echo"}}]}}`)
	events, err = parser.ParseLine(testThread, other)
	if err != nil {
		t.Fatalf("parse other: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("other parent should emit subagent_model + tool start, got %d", len(events))
	}
	if events[0].ItemID != "parent_tool_other" {
		t.Errorf("other parent meta should target parent_tool_other; got %q", events[0].ItemID)
	}
}

func TestParseAssistantSkipsSubagentModelWhenAbsent(t *testing.T) {
	// Envelopes without `message.model` (test fixtures, malformed
	// frames) must not emit a meta event with an empty model — that
	// would write `{"subagent_model":""}` onto the parent and
	// confuse downstream renderers.
	line := []byte(`{"type":"assistant","parent_tool_use_id":"parent_tool_123","message":{"id":"msg-no-model","role":"assistant","content":[{"type":"tool_use","id":"inner","name":"Bash","input":{"command":"ls"}}]}}`)

	events, err := NewParser().ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event (just the tool start), got %d", len(events))
	}
	if events[0].ItemID != "inner" {
		t.Errorf("expected inner tool start; got %q", events[0].ItemID)
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
