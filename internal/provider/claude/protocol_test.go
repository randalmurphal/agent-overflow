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

func TestParseLine_UserType(t *testing.T) {
	line := []byte(`{"type":"user","message":{"role":"user","content":"test"}}`)
	events, err := ParseLine(testThreadProto, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events for user type, got %d", len(events))
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
