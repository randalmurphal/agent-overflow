package claude

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

const testThreadProto = "thread-proto"

// -- ParseLine tests --

func TestParseLine_SystemInit(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"init","session_id":"s1","model":"claude-opus-4-6","cwd":"/tmp"}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventInit {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventInit)
	}
}

// TestParseLine_SystemInitSlashCommands exercises the slash_commands array on
// system.init. The Claude CLI surfaces user-configurable slash commands (from
// .claude/commands/ and built-ins) here; we round-trip them through
// SessionInfo so the frontend composer can render an autocomplete popover.
func TestParseLine_SystemInitSlashCommands(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"init","session_id":"s1","slash_commands":["init","review","deploy-staging"]}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	var info provider.SessionInfo
	if err := json.Unmarshal(events[0].Meta, &info); err != nil {
		t.Fatalf("unmarshal session info: %v", err)
	}
	want := []string{"init", "review", "deploy-staging"}
	if len(info.SlashCommands) != len(want) {
		t.Fatalf("SlashCommands len = %d, want %d (%v)", len(info.SlashCommands), len(want), info.SlashCommands)
	}
	for i, v := range want {
		if info.SlashCommands[i] != v {
			t.Errorf("SlashCommands[%d] = %q, want %q", i, info.SlashCommands[i], v)
		}
	}
}

// TestParseLine_SystemInitWithoutSlashCommands guards against a CLI payload
// that omits slash_commands entirely — the field must remain nil/empty and
// parsing must still succeed.
func TestParseLine_SystemInitWithoutSlashCommands(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"init","session_id":"s1","model":"claude-opus-4-6"}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	var info provider.SessionInfo
	if err := json.Unmarshal(events[0].Meta, &info); err != nil {
		t.Fatalf("unmarshal session info: %v", err)
	}
	if len(info.SlashCommands) != 0 {
		t.Errorf("expected empty SlashCommands, got %v", info.SlashCommands)
	}
}

func TestParseLine_SystemSessionStateChanged(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"session_state_changed","data":{"state":"idle"}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventSessionStatus {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventSessionStatus)
	}
}

func TestParseLine_SystemApiRetry(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"api_retry","data":{"attempt":2}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventSessionStatus {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventSessionStatus)
	}
	if events[0].Content != "retrying" {
		t.Errorf("content: got %q, want %q", events[0].Content, "retrying")
	}
}

func TestParseLine_SystemSkippedSubtypes(t *testing.T) {
	subtypes := []string{
		"hook_started", "hook_progress", "hook_response",
		"notification", "files_persisted", "tool_use_summary",
		"memory_recall", "local_command_output",
	}
	for _, sub := range subtypes {
		line := []byte(`{"type":"system","subtype":"` + sub + `"}`)
		events, err := ParseLine(testThreadProto, line)
		if err != nil {
			t.Errorf("subtype %q: unexpected error: %v", sub, err)
		}
		if len(events) != 0 {
			t.Errorf("subtype %q: expected 0 events, got %d", sub, len(events))
		}
	}
}

func TestParseLine_SystemUnknownSubtype(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"future_feature"}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events for unknown subtype, got %d", len(events))
	}
}

func TestParseLine_AssistantText(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","content":[{"type":"text","text":"Hello"}],"role":"assistant"}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventTextDelta {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventTextDelta)
	}
	if events[0].Content != "Hello" {
		t.Errorf("content: got %q, want %q", events[0].Content, "Hello")
	}
}

func TestParseLine_AssistantToolUse(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","content":[{"type":"tool_use","id":"tool-1","name":"Bash","input":{"command":"ls"}}],"role":"assistant"}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventToolStart {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventToolStart)
	}
	if events[0].ItemType != "Bash" {
		t.Errorf("itemType: got %q, want %q", events[0].ItemType, "Bash")
	}
}

func TestParseLine_AssistantWithUsage(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","content":[],"role":"assistant","usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":200,"cache_creation_input_tokens":30}}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	hasUsage := false
	for _, evt := range events {
		if evt.Kind == provider.EventTokenUsage {
			hasUsage = true
			var usage provider.TokenUsage
			if err := json.Unmarshal(evt.Meta, &usage); err != nil {
				t.Fatalf("unmarshal usage: %v", err)
			}
			if usage.InputTokens != 100 {
				t.Errorf("InputTokens: got %d, want 100", usage.InputTokens)
			}
			if usage.CacheCreationInputTokens != 30 {
				t.Errorf("CacheCreationInputTokens: got %d, want 30", usage.CacheCreationInputTokens)
			}
		}
	}
	if !hasUsage {
		t.Error("expected a token usage event")
	}
}

// TestParseLine_UserPlainContent confirms that a `user` message whose
// `content` is a plain string (the echo of a user-typed turn input rather
// than a tool_result echo) produces no events. Only tool_result blocks in a
// list-shaped content are meaningful at this layer.
func TestParseLine_UserPlainContent(t *testing.T) {
	line := []byte(`{"type":"user","message":{"role":"user","content":"test"}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events for user type, got %d", len(events))
	}
}

// TestParseLine_AssistantToolUseNoBackgroundMetaByDefault verifies that a
// tool_use block without `run_in_background` does not add `is_background`
// to the emitted EventToolStart's Meta. Absence is the default.
func TestParseLine_AssistantToolUseNoBackgroundMetaByDefault(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"tool-fg","name":"Bash","input":{"command":"ls"}}]}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if v, ok := meta["is_background"]; ok && v != false {
		t.Errorf("expected is_background to be absent or false, got %v", v)
	}
}

// TestParseLine_AssistantToolUseRunInBackground verifies that a tool_use
// with `run_in_background: true` surfaces `is_background: true` on the
// emitted EventToolStart's Meta.
func TestParseLine_AssistantToolUseRunInBackground(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"tool-bg","name":"Bash","input":{"command":"npm run dev","run_in_background":true}}]}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventToolStart {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventToolStart)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["is_background"] != true {
		t.Errorf("expected is_background=true, got %v", meta["is_background"])
	}
}

// TestParseLine_UserToolResultEmitsComplete verifies that a user message
// with a `tool_result` block emits a matching EventToolComplete keyed by
// the original tool_use_id.
func TestParseLine_UserToolResultEmitsComplete(t *testing.T) {
	line := []byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":"file listing","is_error":false}]}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	evt := events[0]
	if evt.Kind != provider.EventToolComplete {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventToolComplete)
	}
	if evt.ItemID != "tool-1" {
		t.Errorf("itemID: got %q, want %q", evt.ItemID, "tool-1")
	}
	if evt.Content != "file listing" {
		t.Errorf("content: got %q, want %q", evt.Content, "file listing")
	}
	var meta map[string]any
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["is_error"] != false {
		t.Errorf("expected is_error=false, got %v", meta["is_error"])
	}
	if _, ok := meta["is_background"]; ok {
		t.Errorf("expected is_background absent for foreground tool, got %v", meta["is_background"])
	}
}

// TestParseLine_UserToolResultMultipleBlocks verifies that a user message
// carrying multiple tool_result blocks emits one EventToolComplete for each
// block, with each ItemID mapped back to its origin tool_use_id.
func TestParseLine_UserToolResultMultipleBlocks(t *testing.T) {
	line := []byte(`{"type":"user","message":{"role":"user","content":[
		{"type":"tool_result","tool_use_id":"tool-a","content":"first"},
		{"type":"tool_result","tool_use_id":"tool-b","content":"second"}
	]}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	want := map[string]string{"tool-a": "first", "tool-b": "second"}
	for _, evt := range events {
		if evt.Kind != provider.EventToolComplete {
			t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventToolComplete)
		}
		expected, ok := want[evt.ItemID]
		if !ok {
			t.Errorf("unexpected tool_use_id %q", evt.ItemID)
			continue
		}
		if evt.Content != expected {
			t.Errorf("content for %s: got %q, want %q", evt.ItemID, evt.Content, expected)
		}
	}
}

// TestParseLine_UserToolResultError verifies that `is_error: true` on the
// tool_result block is reflected in the emitted EventToolComplete's Meta.
func TestParseLine_UserToolResultError(t *testing.T) {
	line := []byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":"command not found","is_error":true}]}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["is_error"] != true {
		t.Errorf("expected is_error=true, got %v", meta["is_error"])
	}
}

// TestParseLine_UserToolResultStructuredContent verifies that a tool_result
// whose content is a list of text blocks (the richer shape some tools emit)
// is flattened into a single Content string.
func TestParseLine_UserToolResultStructuredContent(t *testing.T) {
	line := []byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":[{"type":"text","text":"hello "},{"type":"text","text":"world"}]}]}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Content != "hello world" {
		t.Errorf("content: got %q, want %q", events[0].Content, "hello world")
	}
}

// TestParser_BackgroundToolUsePropagatesToToolResult verifies the
// cross-line correlation: a Parser instance parses a tool_use with
// `run_in_background: true`, then a subsequent user tool_result for the
// same tool_use_id. The EventToolComplete must carry `is_background: true`.
func TestParser_BackgroundToolUsePropagatesToToolResult(t *testing.T) {
	parser := NewParser()

	startLine := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"tool-bg","name":"Bash","input":{"command":"npm run dev","run_in_background":true}}]}}`)
	startEvents, err := parser.ParseLine(testThreadProto, startLine)
	if err != nil {
		t.Fatalf("parse start: %v", err)
	}
	if len(startEvents) != 1 {
		t.Fatalf("start: expected 1 event, got %d", len(startEvents))
	}
	var startMeta map[string]any
	if err := json.Unmarshal(startEvents[0].Meta, &startMeta); err != nil {
		t.Fatalf("unmarshal start meta: %v", err)
	}
	if startMeta["is_background"] != true {
		t.Fatalf("start: expected is_background=true, got %v", startMeta["is_background"])
	}

	completeLine := []byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-bg","content":"dev server launched"}]}}`)
	completeEvents, err := parser.ParseLine(testThreadProto, completeLine)
	if err != nil {
		t.Fatalf("parse complete: %v", err)
	}
	if len(completeEvents) != 1 {
		t.Fatalf("complete: expected 1 event, got %d", len(completeEvents))
	}
	if completeEvents[0].Kind != provider.EventToolComplete {
		t.Errorf("complete kind: got %q, want %q", completeEvents[0].Kind, provider.EventToolComplete)
	}
	if completeEvents[0].ItemID != "tool-bg" {
		t.Errorf("complete itemID: got %q, want %q", completeEvents[0].ItemID, "tool-bg")
	}
	var completeMeta map[string]any
	if err := json.Unmarshal(completeEvents[0].Meta, &completeMeta); err != nil {
		t.Fatalf("unmarshal complete meta: %v", err)
	}
	if completeMeta["is_background"] != true {
		t.Errorf("complete: expected is_background=true, got %v", completeMeta["is_background"])
	}
}

// TestParser_ForegroundToolResultHasNoBackgroundFlag confirms the negative
// case: a tool_use without run_in_background followed by its tool_result
// produces a complete event whose Meta omits is_background (or has it
// false).
func TestParser_ForegroundToolResultHasNoBackgroundFlag(t *testing.T) {
	parser := NewParser()

	startLine := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"tool-fg","name":"Bash","input":{"command":"ls"}}]}}`)
	if _, err := parser.ParseLine(testThreadProto, startLine); err != nil {
		t.Fatalf("parse start: %v", err)
	}

	completeLine := []byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-fg","content":"file1\nfile2"}]}}`)
	completeEvents, err := parser.ParseLine(testThreadProto, completeLine)
	if err != nil {
		t.Fatalf("parse complete: %v", err)
	}
	if len(completeEvents) != 1 {
		t.Fatalf("complete: expected 1 event, got %d", len(completeEvents))
	}
	var meta map[string]any
	if err := json.Unmarshal(completeEvents[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if v, ok := meta["is_background"]; ok && v != false {
		t.Errorf("expected is_background absent or false for foreground tool, got %v", v)
	}
}

// TestParser_ExitCodeSurfacedFromToolUseResult verifies that when Claude
// attaches a structured `tool_use_result` sibling with an `exit_code`
// (typical for Bash), the EventToolComplete Meta exposes it so downstream
// UI can flag command failures without re-parsing the text body.
func TestParser_ExitCodeSurfacedFromToolUseResult(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":"boom","is_error":true}]},"tool_use_result":{"tool_use_id":"tool-1","exit_code":127,"stdout":"","stderr":"boom"}}`)

	events, err := parser.ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["exit_code"] != float64(127) {
		t.Errorf("expected exit_code=127, got %v", meta["exit_code"])
	}
	if meta["is_error"] != true {
		t.Errorf("expected is_error=true, got %v", meta["is_error"])
	}
}

func TestParseLine_ResultSuccess(t *testing.T) {
	line := []byte(`{"type":"result","is_error":false,"session_id":"s1"}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventTurnComplete {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventTurnComplete)
	}
}

func TestParseLine_ResultError(t *testing.T) {
	line := []byte(`{"type":"result","is_error":true,"error":"something failed"}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventError {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventError)
	}
	if events[0].Content != "something failed" {
		t.Errorf("content: got %q, want %q", events[0].Content, "something failed")
	}
}

func TestParseLine_StreamEventTextDelta(t *testing.T) {
	line := []byte(`{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":"text_delta","text":"world"}}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Content != "world" {
		t.Errorf("content: got %q, want %q", events[0].Content, "world")
	}
}

func TestParseLine_StreamEventNoDelta(t *testing.T) {
	line := []byte(`{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start"}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events for stream event without delta text, got %d", len(events))
	}
}

func TestParseLine_ControlRequestCanUseTool(t *testing.T) {
	line := []byte(`{"type":"control_request","request_id":"req-1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventApprovalRequest {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventApprovalRequest)
	}
	if events[0].ItemID != "req-1" {
		t.Errorf("itemID: got %q, want %q", events[0].ItemID, "req-1")
	}

	var approval provider.ApprovalRequest
	if err := json.Unmarshal(events[0].Meta, &approval); err != nil {
		t.Fatalf("unmarshal approval: %v", err)
	}
	if approval.ToolName != "Bash" {
		t.Errorf("toolName: got %q, want %q", approval.ToolName, "Bash")
	}
}

func TestParseLine_ControlRequestNonToolSubtype(t *testing.T) {
	line := []byte(`{"type":"control_request","request_id":"req-2","request":{"subtype":"other_subtype"}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events for non-tool control request, got %d", len(events))
	}
}

func TestParseLine_UnknownType(t *testing.T) {
	line := []byte(`{"type":"rate_limit_event","data":{}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events for unknown type, got %d", len(events))
	}
}

func TestParseLine_InvalidJSON(t *testing.T) {
	_, err := ParseLine(testThreadProto, []byte(`not json`))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestParseLine_MissingType(t *testing.T) {
	_, err := ParseLine(testThreadProto, []byte(`{"data":"something"}`))
	if err == nil {
		t.Error("expected error for missing type field")
	}
}

func TestParseLine_SystemToolProgress(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"tool_progress","item_id":"item-1","progress":{"percent":50}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventToolProgress {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventToolProgress)
	}
	if events[0].ItemID != "item-1" {
		t.Errorf("itemID: got %q, want %q", events[0].ItemID, "item-1")
	}
}

func TestParseLine_SystemCompactBoundary(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"compact_boundary","data":{"usedTokens":5000,"maxTokens":200000}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventCompactBoundary {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventCompactBoundary)
	}
}
