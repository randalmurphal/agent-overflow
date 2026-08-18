package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

const testThread = "thread-test"

func TestParseSystemInit(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"init","session_id":"abc-123","model":"claude-opus-4-6","cwd":"/home/user","tools":["Bash","Edit"],"claude_code_version":"2.0.0"}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	evt := events[0]
	if evt.Kind != provider.EventInit {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventInit)
	}
	if evt.ThreadID != testThread {
		t.Errorf("threadID: got %q, want %q", evt.ThreadID, testThread)
	}

	var info provider.SessionInfo
	if err := json.Unmarshal(evt.Meta, &info); err != nil {
		t.Fatalf("unmarshal session info: %v", err)
	}
	if info.SessionID != "abc-123" {
		t.Errorf("sessionID: got %q, want %q", info.SessionID, "abc-123")
	}
	if info.Model != "claude-opus-4-6" {
		t.Errorf("model: got %q, want %q", info.Model, "claude-opus-4-6")
	}
	if info.CWD != "/home/user" {
		t.Errorf("cwd: got %q, want %q", info.CWD, "/home/user")
	}
	if len(info.Tools) != 2 {
		t.Errorf("tools: got %d, want 2", len(info.Tools))
	}
	if info.Version != "2.0.0" {
		t.Errorf("version: got %q, want %q", info.Version, "2.0.0")
	}
}

func TestParseAssistantTextBlockIsSkipped(t *testing.T) {
	// Text blocks on the coalesced `assistant` envelope are skipped —
	// the same content streams through stream_event content_block_delta
	// (covered by TestParseStreamEventTextDelta). Emitting from both
	// paths would double the cumulative summary in triage.
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":"Hello world"}]}}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, e := range events {
		if e.Kind == provider.EventTextDelta {
			t.Fatalf("assistant envelope emitted EventTextDelta for a text block: %+v", e)
		}
	}
}

func TestParseAssistantToolUseBlock(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"Bash","input":{"command":"ls"}}]}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	evt := events[0]
	if evt.Kind != provider.EventToolStart {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventToolStart)
	}
	if evt.ItemID != "tool-1" {
		t.Errorf("itemID: got %q, want %q", evt.ItemID, "tool-1")
	}
	if evt.ItemType != "Bash" {
		t.Errorf("itemType: got %q, want %q", evt.ItemType, "Bash")
	}

	var meta map[string]any
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["toolName"] != "Bash" {
		t.Errorf("meta toolName: got %v, want %q", meta["toolName"], "Bash")
	}
}

func TestParseAssistantExitPlanModeBlock(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"tool-plan-1","name":"ExitPlanMode","input":{"plan":"# Final plan\n\n- ship it"}}]}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	evt := events[0]
	if evt.Kind != provider.EventProposedPlan {
		t.Fatalf("kind: got %q, want %q", evt.Kind, provider.EventProposedPlan)
	}
	if evt.Content != "# Final plan\n\n- ship it" {
		t.Fatalf("content: got %q, want %q", evt.Content, "# Final plan\n\n- ship it")
	}
}

// assistantMessageStartLine builds the stream_event.message_start that
// precedes a normal streamed assistant message. Its id marks the message
// as already-streamed, so the coalesced `assistant` snapshot carrying the
// same id drops its text/thinking blocks (anti-double-render) instead of
// recovering them. Production always emits this before a snapshot
// (--include-partial-messages); the never-streamed recovery path is
// covered by the partial_messages_test.go suite.
func assistantMessageStartLine(id string) []byte {
	return []byte(`{"type":"stream_event","event":"message_start","data":{"type":"message_start","message":{"id":"` + id + `","role":"assistant","content":[]}}}`)
}

func TestParseAssistantThinkingBlockIsSkipped(t *testing.T) {
	// A thinking block on the coalesced `assistant` envelope is dropped
	// when it already streamed (stream_event thinking_delta is the sole
	// source of thinking content). The snapshot must not re-emit it on any
	// channel — neither a streaming EventThinking nor a completed block.
	parser := NewParser()
	if _, err := parser.ParseLine(testThread, assistantMessageStartLine("msg-1")); err != nil {
		t.Fatalf("parse message_start: %v", err)
	}
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"thinking","thinking":"Let me consider..."}]}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, e := range events {
		if e.Kind == provider.EventThinking {
			t.Fatalf("assistant envelope emitted EventThinking for a thinking block: %+v", e)
		}
		if e.Kind == provider.EventContentBlockStop && e.ContentPresent {
			t.Fatalf("already-streamed thinking re-emitted as a completed block: %+v", e)
		}
	}
}

func TestParseAssistantWithUsage(t *testing.T) {
	// Text is dropped (already streamed — message_start marks it); usage is
	// still emitted from the assistant envelope.
	parser := NewParser()
	if _, err := parser.ParseLine(testThread, assistantMessageStartLine("msg-1")); err != nil {
		t.Fatalf("parse message_start: %v", err)
	}
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":80,"cache_creation_input_tokens":20}}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event (usage only), got %d: %+v", len(events), events)
	}
	if events[0].Kind != provider.EventTokenUsage {
		t.Errorf("kind: got %q, want token_usage", events[0].Kind)
	}
	var usage provider.ContextWindow
	if err := json.Unmarshal(events[0].Meta, &usage); err != nil {
		t.Fatalf("unmarshal usage: %v", err)
	}
	if usage.UsedTokens != 200 {
		t.Errorf("used tokens: got %d, want 200", usage.UsedTokens)
	}
}

func TestParseAssistantMultipleBlocks(t *testing.T) {
	// Thinking and text are dropped on the assistant envelope (already
	// streamed — message_start marks them). Only the tool_use fires.
	parser := NewParser()
	if _, err := parser.ParseLine(testThread, assistantMessageStartLine("msg-1")); err != nil {
		t.Fatalf("parse message_start: %v", err)
	}
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"hello"},{"type":"tool_use","id":"t1","name":"Edit","input":{"file":"x"}}]}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event (tool_use only), got %d: %+v", len(events), events)
	}
	if events[0].Kind != provider.EventToolStart {
		t.Errorf("kind: got %q, want tool_start", events[0].Kind)
	}
}

func TestParseSystemSkippedSubtypes(t *testing.T) {
	skipped := []string{
		"hook_started", "hook_progress", "hook_response",
		"notification", "files_persisted",
		"tool_use_summary", "memory_recall", "local_command_output",
	}

	for _, subtype := range skipped {
		line := []byte(`{"type":"system","subtype":"` + subtype + `"}`)
		events, err := ParseLine(testThread, line)
		if err != nil {
			t.Errorf("subtype %q: unexpected error: %v", subtype, err)
		}
		if len(events) != 0 {
			t.Errorf("subtype %q: expected 0 events, got %d", subtype, len(events))
		}
	}
}

func TestParseToolProgressDropped(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"tool_progress","item_id":"item-1","content":{"progress":{"current":5,"total":10,"message":"Reading..."}}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected tool_progress to be dropped, got %d event(s)", len(events))
	}
}

func TestParseToolProgressNoContentDropped(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"tool_progress"}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected tool_progress to be dropped, got %d event(s)", len(events))
	}
}

func TestParseCompactBoundary(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"compact_boundary","uuid":"compact-1","content":"Conversation compacted","data":{"context_window":{"used_tokens":50000,"max_tokens":200000,"used_percentage":25,"total_processed":120000}}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	evt := events[0]
	if evt.Kind != provider.EventCompactBoundary {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventCompactBoundary)
	}
	if evt.ThreadID != testThread {
		t.Errorf("threadID: got %q, want %q", evt.ThreadID, testThread)
	}
	if evt.ItemID != "compact-1" {
		t.Errorf("itemID: got %q, want compact-1", evt.ItemID)
	}
	if evt.Content != "Conversation compacted" {
		t.Errorf("content: got %q, want Conversation compacted", evt.Content)
	}

	var meta provider.ContextWindow
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta.UsedTokens != 50000 {
		t.Errorf("UsedTokens: got %d, want 50000", meta.UsedTokens)
	}
	if meta.MaxTokens != 200000 {
		t.Errorf("MaxTokens: got %d, want 200000", meta.MaxTokens)
	}
	if meta.UsedPercentage != 25 {
		t.Errorf("UsedPercentage: got %f, want 25", meta.UsedPercentage)
	}
}

func TestParseCompactBoundaryPreservesCompactMetadata(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"compact_boundary","uuid":"compact-2","content":"Conversation compacted","compactMetadata":{"trigger":"auto","durationMs":111814}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	evt := events[0]
	if evt.ItemID != "compact-2" {
		t.Errorf("itemID: got %q, want compact-2", evt.ItemID)
	}
	if evt.Content != "Conversation compacted" {
		t.Errorf("content: got %q, want Conversation compacted", evt.Content)
	}
	var meta map[string]any
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["trigger"] != "auto" {
		t.Fatalf("trigger = %v, want auto", meta["trigger"])
	}
	if meta["durationMs"] != float64(111814) {
		t.Fatalf("durationMs = %v, want 111814", meta["durationMs"])
	}
}

func TestParseCompactBoundaryPreservesCompactMetadataWhenDataIsNotContextWindow(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"compact_boundary","uuid":"compact-3","content":"Conversation compacted","data":{"note":"not a context window"},"compactMetadata":{"trigger":"auto","durationMs":222}}`)

	events, err := ParseLine(testThread, line)
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
	if meta["trigger"] != "auto" {
		t.Fatalf("trigger = %v, want auto", meta["trigger"])
	}
	if meta["durationMs"] != float64(222) {
		t.Fatalf("durationMs = %v, want 222", meta["durationMs"])
	}
}

func TestParseCompactBoundaryNoData(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"compact_boundary"}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	evt := events[0]
	if evt.Kind != provider.EventCompactBoundary {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventCompactBoundary)
	}
	if evt.Meta != nil {
		t.Errorf("meta: got %s, want nil", evt.Meta)
	}
}

func TestParseApiRetry(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"api_retry","data":{"attempt":2,"max_retries":10,"retry_after_ms":5000,"error":{"message":"server overloaded"}}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	evt := events[0]
	if evt.Kind != provider.EventAPIRetry {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventAPIRetry)
	}
	if evt.ThreadID != testThread {
		t.Errorf("threadID: got %q, want %q", evt.ThreadID, testThread)
	}

	var meta map[string]any
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["attempt"] != float64(2) {
		t.Errorf("attempt: got %v, want 2", meta["attempt"])
	}
	if meta["max_retries"] != float64(10) {
		t.Errorf("max_retries: got %v, want 10", meta["max_retries"])
	}
	if meta["retry_after_ms"] != float64(5000) {
		t.Errorf("retry_after_ms: got %v, want 5000", meta["retry_after_ms"])
	}
	if meta["error"] != "server overloaded" {
		t.Errorf("error: got %v, want \"server overloaded\"", meta["error"])
	}
}

func TestParseUnknownSystemSubtype(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"future_feature"}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events, got %d", len(events))
	}
}

func TestParseInvalidJSON(t *testing.T) {
	_, err := ParseLine(testThread, []byte(`not json`))
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestParseResultSuccess(t *testing.T) {
	line := []byte(`{"type":"result","subtype":"success","is_error":false,"result":"Done","session_id":"s1"}`)

	events, err := ParseLine(testThread, line)
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

// TestParseResult_IsErrorTrueEmitsTurnComplete pins the post-cleanup
// behaviour: an `is_error: true` result no longer branches into an
// EventError. The Python SDK's SDKResultError shape has no bare
// `error` field — only `errors: string[]` — so the previous branch
// always produced an EventError with empty content. The branch was
// dead (docs/references/claude-wire.md §result). Interrupted turns
// are still detected via detectInterrupted; other error subtypes
// settle as a normal EventTurnComplete whose stop_reason carries
// the shape signal.
func TestParseResult_IsErrorTrueWithSubtypeEmitsTurnComplete(t *testing.T) {
	line := []byte(`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["something broke"]}`)

	events, err := ParseLine(testThread, line)
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

func TestParseStreamEvent(t *testing.T) {
	line := []byte(`{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello "}}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventTextDelta {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventTextDelta)
	}
	if events[0].Content != "hello " {
		t.Errorf("content: got %q, want %q", events[0].Content, "hello ")
	}
}

func TestParseStreamEventMessageBoundariesDoNotEmitLifecycleEvents(t *testing.T) {
	for _, line := range [][]byte{
		[]byte(`{"type":"stream_event","event":"message_start","data":{"type":"message_start"}}`),
		[]byte(`{"type":"stream_event","event":"message_stop","data":{"type":"message_stop"}}`),
	} {
		events, err := ParseLine(testThread, line)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(events) != 0 {
			t.Errorf("expected 0 events, got %d", len(events))
		}
	}
}

func TestParseControlRequestCanUseTool(t *testing.T) {
	line := []byte(`{"type":"control_request","request_id":"req_1_abc","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"rm -rf /"}}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	evt := events[0]
	if evt.Kind != provider.EventApprovalRequest {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventApprovalRequest)
	}
	if evt.ItemID != "req_1_abc" {
		t.Errorf("itemID: got %q, want %q", evt.ItemID, "req_1_abc")
	}

	var approval provider.ApprovalRequest
	if err := json.Unmarshal(evt.Meta, &approval); err != nil {
		t.Fatalf("unmarshal approval: %v", err)
	}
	if approval.RequestID != "req_1_abc" {
		t.Errorf("requestID: got %q, want %q", approval.RequestID, "req_1_abc")
	}
	if approval.ToolName != "Bash" {
		t.Errorf("toolName: got %q, want %q", approval.ToolName, "Bash")
	}
	if string(approval.Input) != `{"command":"rm -rf /"}` {
		t.Errorf("input: got %s, want %s", approval.Input, `{"command":"rm -rf /"}`)
	}
}

func TestParseControlRequestAskUserQuestionNormalizesQuestionIDs(t *testing.T) {
	line := []byte(`{"type":"control_request","request_id":"req-ask","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":[{"header":"Framework","question":"Pick one","options":[{"label":"React","description":""}]},{"question":"Pick mode","options":[{"label":"Plan","description":""}]},{"id":"__proto__","header":"constructor","question":"Reserved","options":[{"label":"Safe","description":""}]}]}}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventUserInputRequest {
		t.Fatalf("kind: got %q, want %q", events[0].Kind, provider.EventUserInputRequest)
	}

	var request provider.UserInputRequest
	if err := json.Unmarshal(events[0].Meta, &request); err != nil {
		t.Fatalf("unmarshal user input: %v", err)
	}
	if len(request.Questions) != 3 {
		t.Fatalf("questions len = %d, want 3", len(request.Questions))
	}
	got := []string{request.Questions[0].ID, request.Questions[1].ID, request.Questions[2].ID}
	want := []string{"Framework", "Pick mode", "Reserved"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("question IDs = %#v, want %#v", got, want)
	}
}

func TestParseControlRequestMalformedAskUserQuestionFallsBackToApproval(t *testing.T) {
	line := []byte(`{"type":"control_request","request_id":"req-bad-ask","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":[]}}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventApprovalRequest {
		t.Fatalf("kind: got %q, want %q", events[0].Kind, provider.EventApprovalRequest)
	}
}

func TestParseControlRequestUnknownSubtype(t *testing.T) {
	line := []byte(`{"type":"control_request","request_id":"req_1","request":{"subtype":"unknown_subtype"}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events, got %d", len(events))
	}
}

// TestParseLine_ControlResponseSuccess confirms that a control_response
// line (the CLI's reply to our outbound stop_task control_request) is
// routed silently through the top-level dispatch: no ProviderEvent is
// emitted, the line isn't misclassified as a control_request, and no
// parse error surfaces. The session-level readLoop handles routing to
// pending StopTask callers via handleControlResponseLine; ParseLine's
// job is just to not mangle the envelope.
func TestParseLine_ControlResponseSuccess(t *testing.T) {
	successLine := []byte(`{"type":"control_response","response":{"subtype":"success","request_id":"so-1","response":{}}}`)
	events, err := ParseLine(testThread, successLine)
	if err != nil {
		t.Fatalf("parse success: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("control_response must emit 0 events, got %d: %+v", len(events), events)
	}

	// Error form must parse cleanly too — the parser never mistakes it
	// for an inbound control_request (which would misroute it into
	// parseControlRequest as a malformed approval).
	errorLine := []byte(`{"type":"control_response","response":{"subtype":"error","request_id":"so-2","error":"boom"}}`)
	events, err = ParseLine(testThread, errorLine)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("control_response(error) must emit 0 events, got %d: %+v", len(events), events)
	}
}

func TestParseUnknownType(t *testing.T) {
	line := []byte(`{"type":"some_future_event","data":{}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events for unknown type, got %d", len(events))
	}
}

func TestParseUserTypeSkipped(t *testing.T) {
	line := []byte(`{"type":"user","message":{"role":"user","content":"hi"}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events for user type, got %d", len(events))
	}
}

// TestParseRealCLIFixture validates the parser against real Claude CLI
// output. The fixture predates `--include-partial-messages`, so text
// reaches triage via `stream_event` deltas now rather than via the
// coalesced `assistant` envelope — the `EventTextDelta` check that
// previously ran here has moved to the unit tests in
// `partial_messages_test.go`. This test's bar is "the parser handles
// every line without error and recognises the session bookends."
func TestParseRealCLIFixture(t *testing.T) {
	f, err := os.Open("testdata/real_output.ndjson")
	if err != nil {
		t.Skipf("skipping real fixture test: %v", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	var lineNum int
	var foundInit, foundResult bool

	for scanner.Scan() {
		lineNum++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		events, err := ParseLine(testThread, line)
		if err != nil {
			t.Errorf("line %d: parse error: %v", lineNum, err)
			continue
		}

		for _, evt := range events {
			switch evt.Kind {
			case provider.EventInit:
				foundInit = true
				var info provider.SessionInfo
				if err := json.Unmarshal(evt.Meta, &info); err != nil {
					t.Errorf("line %d: unmarshal session info: %v", lineNum, err)
				}
				if info.SessionID == "" {
					t.Errorf("line %d: init event has empty session ID", lineNum)
				}
				if info.Model == "" {
					t.Errorf("line %d: init event has empty model", lineNum)
				}
			case provider.EventTurnComplete:
				foundResult = true
			}
		}
	}

	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner error: %v", err)
	}

	if !foundInit {
		t.Error("fixture missing system/init event")
	}
	if !foundResult {
		t.Error("fixture missing result/turn_complete event")
	}

	t.Logf("processed %d lines from real fixture: init=%v result=%v",
		lineNum, foundInit, foundResult)
}

// -- Session unit tests (wire format verification) --

func TestBuildArgsDefault(t *testing.T) {
	args := buildArgs(Config{}, "")

	// Baseline flags that every spawn must include. Adding a new flag to
	// buildArgs should extend this list intentionally.
	expected := []string{
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--permission-prompt-tool", "stdio",
		"--include-partial-messages",
		"--replay-user-messages",
		"--thinking-display", "summarized",
	}
	if len(args) != len(expected) {
		t.Fatalf("expected %d args, got %d: %v", len(expected), len(args), args)
	}
	for i, want := range expected {
		if args[i] != want {
			t.Errorf("args[%d]: got %q, want %q", i, args[i], want)
		}
	}
}

func TestBuildArgsOmitsResumeAtWithoutResume(t *testing.T) {
	args := buildArgs(Config{ResumeAt: "leaf-456"}, "")
	for _, arg := range args {
		if arg == "--resume-session-at" {
			t.Fatalf("args include --resume-session-at without --resume: %v", args)
		}
	}
}

func TestVerifyReplayParentFailsRiskyReplayWithoutVerifiableParent(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "thread-risky",
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
		expectedReplayByUUID: map[string]replayExpectation{
			"user-wire": {parent: "leaf-expected", wasRisky: true},
		},
		expectedReplayOrder: []string{"user-wire"},
	}
	meta, _ := json.Marshal(map[string]string{
		"provider_item_id": "user-wire",
	})

	s.verifyReplayParent(provider.ProviderEvent{
		Kind: provider.EventUserText,
		Meta: meta,
	})

	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].Kind != provider.EventError {
		t.Fatalf("event kind = %q, want error", events[0].Kind)
	}
	if !strings.Contains(events[0].Content, "could not verify") {
		t.Fatalf("error content = %q, want verification failure", events[0].Content)
	}
}

func TestBuildArgsWithAllOptions(t *testing.T) {
	cfg := Config{
		Model:           "opus",
		Resume:          "session-123",
		ResumeAt:        "leaf-456",
		ForkSession:     true,
		SystemPrompt:    "Be helpful",
		OutputSchema:    `{"type":"object"}`,
		PermissionFlags: []string{"--permission-mode", "acceptEdits"},
		MaxTurns:        5,
		AllowedTools:    []string{"Bash", "Edit"},
	}
	systemPromptPath, err := WriteSystemPromptFile(cfg.SystemPrompt)
	if err != nil {
		t.Fatalf("WriteSystemPromptFile() error = %v", err)
	}
	t.Cleanup(func() { RemoveSystemPromptFile(systemPromptPath) })
	args := buildArgs(cfg, systemPromptPath)

	// Check that all flags are present.
	findFlag := func(flag, value string) bool {
		for i, a := range args {
			if a == flag && i+1 < len(args) && args[i+1] == value {
				return true
			}
		}
		return false
	}

	if !findFlag("--model", "opus") {
		t.Error("missing --model opus")
	}
	if !findFlag("--resume", "session-123") {
		t.Error("missing --resume session-123")
	}
	if !findFlag("--resume-session-at", "leaf-456") {
		t.Error("missing --resume-session-at leaf-456")
	}
	foundForkFlag := false
	for _, arg := range args {
		if arg == "--fork-session" {
			foundForkFlag = true
			break
		}
	}
	if !foundForkFlag {
		t.Error("missing --fork-session")
	}
	// The prompt travels by path, never in argv (MAX_ARG_STRLEN + /proc —
	// see WriteSystemPromptFile), so the flag is only half the assertion:
	// what the CLI will actually read is the file's content.
	if !findFlag("--system-prompt-file", systemPromptPath) {
		t.Errorf("missing --system-prompt-file %s: %v", systemPromptPath, args)
	}
	if slices.Contains(args, "--system-prompt") {
		t.Errorf("argv still carries --system-prompt: %v", args)
	}
	written, err := os.ReadFile(systemPromptPath)
	if err != nil {
		t.Fatalf("read system prompt file: %v", err)
	}
	if string(written) != "Be helpful" {
		t.Errorf("system prompt file content = %q, want %q", written, "Be helpful")
	}
	if !findFlag("--json-schema", `{"type":"object"}`) {
		t.Error("missing --json-schema inline JSON")
	}
	if !findFlag("--permission-mode", "acceptEdits") {
		t.Error("missing --permission-mode acceptEdits")
	}
	if !findFlag("--max-turns", "5") {
		t.Error("missing --max-turns 5")
	}
	if !findFlag("--allowedTools", "Bash") {
		t.Error("missing --allowedTools Bash")
	}
	if !findFlag("--allowedTools", "Edit") {
		t.Error("missing --allowedTools Edit")
	}
}

// The system-prompt file is the one spawn artifact AO leaves on disk, so
// its two properties are pinned: a session without an override writes
// nothing at all, and the file a session DOES write is readable only by the
// user running AO (the prompt carries workspace paths and git state).
func TestWriteSystemPromptFile(t *testing.T) {
	path, err := WriteSystemPromptFile("")
	if err != nil {
		t.Fatalf("WriteSystemPromptFile(\"\") error = %v", err)
	}
	if path != "" {
		RemoveSystemPromptFile(path)
		t.Fatalf("WriteSystemPromptFile(\"\") = %q, want no file for a session with no override", path)
	}
	if args := buildArgs(Config{}, ""); slices.Contains(args, "--system-prompt-file") {
		t.Errorf("argv carries --system-prompt-file without a prompt: %v", args)
	}

	path, err = WriteSystemPromptFile("secret prompt")
	if err != nil {
		t.Fatalf("WriteSystemPromptFile() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat system prompt file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("system prompt file mode = %o, want 0600", perm)
	}

	RemoveSystemPromptFile(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("system prompt file still present after removal: %v", err)
	}
	// Removal is idempotent: Close runs after a failed-spawn path may
	// already have cleaned up.
	RemoveSystemPromptFile(path)
}

func TestBuildArgsNoPermissionFlagsOmitsAll(t *testing.T) {
	args := buildArgs(Config{PermissionFlags: nil}, "")

	for _, a := range args {
		if a == "--permission-mode" || a == "--allow-dangerously-skip-permissions" {
			t.Errorf("permission flag %q should be omitted when PermissionFlags is nil", a)
		}
	}
}

// TestBuildArgsDangerousSkipPermissions confirms the full-access flow emits
// the bypass permission mode plus the bare dangerous-skip allow flag.
func TestBuildArgsDangerousSkipPermissions(t *testing.T) {
	args := buildArgs(Config{PermissionFlags: []string{"--permission-mode", "bypassPermissions", "--allow-dangerously-skip-permissions"}}, "")
	found := false
	for i, a := range args {
		if a != "--allow-dangerously-skip-permissions" {
			continue
		}
		found = true
		// Next arg should not be a companion value — either end of slice
		// or another flag.
		if i+1 < len(args) && !isFlagToken(args[i+1]) {
			t.Errorf("--allow-dangerously-skip-permissions should not carry a value; got %q", args[i+1])
		}
	}
	if !found {
		t.Errorf("expected --allow-dangerously-skip-permissions in args: %v", args)
	}
}

func isFlagToken(arg string) bool { return len(arg) >= 2 && arg[0] == '-' }

func TestSendWireFormat(t *testing.T) {
	// Verify the JSON format matches the Claude CLI input protocol.
	msg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role": "user",
			"content": []map[string]string{
				{
					"type": "text",
					"text": "hello",
				},
			},
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	var msgType string
	json.Unmarshal(parsed["type"], &msgType)
	if msgType != "user" {
		t.Errorf("type: got %q, want %q", msgType, "user")
	}

	var message struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	json.Unmarshal(parsed["message"], &message)
	if message.Role != "user" {
		t.Errorf("role: got %q, want %q", message.Role, "user")
	}
	if len(message.Content) != 1 {
		t.Fatalf("content blocks: got %d, want 1 (%+v)", len(message.Content), message.Content)
	}
	if block := message.Content[0]; block.Type != "text" || block.Text != "hello" {
		t.Errorf("content block: got %+v, want text block %q", block, "hello")
	}
}

func TestRespondToApprovalAllowFormat(t *testing.T) {
	resp := provider.ApprovalResponse{RequestID: "req-1", Decision: "allow"}

	var behavior map[string]any
	if resp.Decision == "allow" || resp.Decision == "allow_session" {
		behavior = map[string]any{"behavior": "allow"}
	} else {
		behavior = map[string]any{"behavior": "deny", "message": "User denied"}
	}
	msg := map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": resp.RequestID,
			"response":   behavior,
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed map[string]json.RawMessage
	json.Unmarshal(data, &parsed)

	var msgType string
	json.Unmarshal(parsed["type"], &msgType)
	if msgType != "control_response" {
		t.Errorf("type: got %q, want %q", msgType, "control_response")
	}

	var response struct {
		Subtype   string `json:"subtype"`
		RequestID string `json:"request_id"`
		Response  struct {
			Behavior string `json:"behavior"`
		} `json:"response"`
	}
	json.Unmarshal(parsed["response"], &response)
	if response.Subtype != "success" {
		t.Errorf("subtype: got %q, want %q", response.Subtype, "success")
	}
	if response.RequestID != "req-1" {
		t.Errorf("request_id: got %q, want %q", response.RequestID, "req-1")
	}
	if response.Response.Behavior != "allow" {
		t.Errorf("behavior: got %q, want %q", response.Response.Behavior, "allow")
	}
}

func TestRespondToApprovalDenyFormat(t *testing.T) {
	resp := provider.ApprovalResponse{RequestID: "req-2", Decision: "deny"}

	var behavior map[string]any
	if resp.Decision == "allow" || resp.Decision == "allow_session" {
		behavior = map[string]any{"behavior": "allow"}
	} else {
		behavior = map[string]any{"behavior": "deny", "message": "User denied"}
	}
	msg := map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": resp.RequestID,
			"response":   behavior,
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed map[string]json.RawMessage
	json.Unmarshal(data, &parsed)

	var response struct {
		Response struct {
			Behavior string `json:"behavior"`
			Message  string `json:"message"`
		} `json:"response"`
	}
	json.Unmarshal(parsed["response"], &response)
	if response.Response.Behavior != "deny" {
		t.Errorf("behavior: got %q, want %q", response.Response.Behavior, "deny")
	}
	if response.Response.Message != "User denied" {
		t.Errorf("message: got %q, want %q", response.Response.Message, "User denied")
	}
}

// TestSession_Interrupt_SuccessRoundTrip drives the happy path end to
// end: Interrupt writes an interrupt control_request, the fake CLI
// matches the request_id and replies with subtype=success, and
// Interrupt returns nil. Implicitly validates the wire envelope shape
// because the fake CLI's stdin matcher only fires for
// `"type":"control_request"` + `"subtype":"interrupt"` together (see
// interruptResponderScript). Pre-fix the malformed envelope wouldn't
// match and this test would time out into the kill fallback instead
// of returning nil.
func TestSession_Interrupt_SuccessRoundTrip(t *testing.T) {
	s := newInterruptResponderSession(t, "success", 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.Interrupt(ctx); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
}

// TestSession_Interrupt_WireEnvelopeShape captures the line written to
// stdin and asserts the exact envelope shape the SDK protocol
// requires: `{"type":"control_request","request_id":"so-N","request":{"subtype":"interrupt"}}`.
// This is a stronger gate than the SuccessRoundTrip test — a future
// regression that changed the envelope structure but happened to
// still contain the magic substrings would slip past the responder
// script but fail this assertion.
func TestSession_Interrupt_WireEnvelopeShape(t *testing.T) {
	capturePath := t.TempDir() + "/interrupt-line.json"
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args: []string{"-c", `
			IFS= read -r line || exit 1
			printf '%s\n' "$line" > "$CAPTURE_PATH"
			reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
			printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"
		`},
		Env: map[string]string{"CAPTURE_PATH": capturePath},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	s := &Session{
		proc:                  proc,
		threadID:              testThread,
		onEvent:               func(provider.ProviderEvent) {},
		cancel:                cancel,
		readDone:              make(chan struct{}),
		controlRequestTimeout: 2 * time.Second,
	}
	go s.readLoop()
	t.Cleanup(func() { _ = s.Close() })

	if err := s.Interrupt(context.Background()); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	data, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read captured line: %v", err)
	}
	var frame struct {
		Type      string `json:"type"`
		RequestID string `json:"request_id"`
		Request   struct {
			Subtype string `json:"subtype"`
		} `json:"request"`
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatalf("unmarshal captured envelope: %v", err)
	}
	if frame.Type != "control_request" {
		t.Errorf("envelope type = %q, want control_request", frame.Type)
	}
	if frame.RequestID == "" {
		t.Errorf("envelope request_id is empty (must be set so control_response can correlate)")
	}
	if frame.Request.Subtype != "interrupt" {
		t.Errorf("envelope request.subtype = %q, want interrupt", frame.Request.Subtype)
	}
}

// TestSession_Interrupt_ErrorResponse confirms that a subtype=error
// response surfaces as a non-nil error whose message contains the
// provider-supplied detail. Negative-asserts that the error is NOT
// classified as a kill so the app layer doesn't accidentally evict
// sessions on benign provider errors (e.g. interrupting between turns
// when no turn is open).
func TestSession_Interrupt_ErrorResponse(t *testing.T) {
	s := newInterruptResponderSession(t, "error", 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := s.Interrupt(ctx)
	if err == nil {
		t.Fatal("Interrupt error: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no active turn") {
		t.Errorf("error should surface provider detail, got: %v", err)
	}
}

// TestSession_Interrupt_TimeoutSurfaces exercises the no-ack path:
// the fake CLI consumes the request and goes silent, Interrupt must
// return a timeout error within the configured window. We deliberately
// do NOT escalate to a process kill — that would also kill backgrounded
// tasks (inverting the documented foreground-only behaviour) and
// silently mask a Claude Code CLI bug. The error surfaces to the user
// as a toast.
func TestSession_Interrupt_TimeoutSurfaces(t *testing.T) {
	s := newInterruptResponderSession(t, "silent", 150*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	err := s.Interrupt(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Interrupt timeout: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("error should mention timeout, got: %v", err)
	}
	if elapsed > 1*time.Second {
		t.Errorf("Interrupt took %s, expected near 150ms (matches sibling StopTask test bound)", elapsed)
	}
	// Session must still be alive — no kill fallback. Backgrounded
	// tasks the user spawned still need to keep running per the wire
	// contract.
	select {
	case <-s.proc.Done():
		t.Fatal("process was killed on Interrupt timeout — should only stop the model, not the session")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestSession_Interrupt_CtxCancelSurfaces confirms the ctx.Done branch
// returns the ctx error to the caller without killing the session.
func TestSession_Interrupt_CtxCancelSurfaces(t *testing.T) {
	s := newInterruptResponderSession(t, "silent", 5*time.Second)
	ctx, cancel := context.WithCancel(context.Background())

	// Cancel after the request is in flight but before any response.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := s.Interrupt(ctx)
	if err == nil {
		t.Fatal("Interrupt ctx-cancel: expected error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error chain should preserve ctx.Canceled, got: %v", err)
	}
	// Session stays alive — see TimeoutSurfaces for the rationale.
	select {
	case <-s.proc.Done():
		t.Fatal("process was killed on ctx-cancel — should only release the request, not stop the session")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestInterruptAckFlowsIntoResultClassification drives the ack
// correlation end to end on a live read loop: Interrupt round-trips
// against a fake CLI that acks and then emits the verbatim 2.1.170
// ede_diagnostic result line (the wire ordering the real CLI uses —
// ack before result, verified 6/6 in the 2026-06-10 spike). The
// resulting EventTurnComplete must classify as a user abort, not a
// hard error — pinning the read-loop handoff
// (handleControlResponseLine → MarkInterruptAcked → parseResult),
// which the parser-only tests can't cover.
func TestInterruptAckFlowsIntoResultClassification(t *testing.T) {
	events := make(chan provider.ProviderEvent, 16)
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args: []string{"-c", `
			IFS= read -r line || exit 1
			reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
			printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"
			printf '%s\n' "$RESULT_LINE"
			sleep 2
		`},
		Env: map[string]string{"RESULT_LINE": ede2_1_170InterruptResultLine},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	s := &Session{
		proc:     proc,
		threadID: testThread,
		parser:   NewParser(),
		onEvent: func(evt provider.ProviderEvent) {
			select {
			case events <- evt:
			default:
			}
		},
		cancel:                cancel,
		readDone:              make(chan struct{}),
		controlRequestTimeout: 2 * time.Second,
	}
	go s.readLoop()
	t.Cleanup(func() { _ = s.Close() })

	if err := s.Interrupt(context.Background()); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case evt := <-events:
			if evt.Kind != provider.EventTurnComplete {
				continue
			}
			meta, ok := evt.TurnComplete.(*provider.WireTurnCompleteMeta)
			if !ok || meta == nil {
				t.Fatalf("TurnComplete payload = %T, want *WireTurnCompleteMeta", evt.TurnComplete)
			}
			if !meta.Aborted {
				t.Fatalf("Aborted = false — interrupt ack did not flow into result classification (StopReason=%q ErrorMessage=%q)", meta.StopReason, meta.ErrorMessage)
			}
			if meta.StopReason != "interrupted" {
				t.Fatalf("StopReason = %q, want interrupted", meta.StopReason)
			}
			if meta.ErrorMessage != "" {
				t.Fatalf("ErrorMessage = %q, want empty", meta.ErrorMessage)
			}
			return
		case <-deadline:
			t.Fatal("no EventTurnComplete within 3s")
		}
	}
}

// -- Session lifecycle tests using cat subprocess --

// newTestClaudeSession creates a Session backed by `cat`, which echoes
// stdin to stdout. This lets us exercise readLoop, Send, etc. without
// a real Claude CLI binary.
func newTestClaudeSession(t *testing.T) (*Session, <-chan provider.ProviderEvent) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: "cat"})
	if err != nil {
		t.Fatalf("spawn cat: %v", err)
	}
	eventCh := make(chan provider.ProviderEvent, 100)
	s := &Session{
		proc:     proc,
		threadID: testThread,
		onEvent: func(evt provider.ProviderEvent) {
			eventCh <- evt
		},
		cancel: cancel,
	}
	go s.readLoop()
	t.Cleanup(func() {
		cancel()
		proc.Close()
	})
	return s, eventCh
}

func waitEvent(t *testing.T, ch <-chan provider.ProviderEvent) provider.ProviderEvent {
	t.Helper()
	select {
	case evt := <-ch:
		return evt
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for event")
		return provider.ProviderEvent{}
	}
}

func waitCapturedLines(t *testing.T, path string, want int) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) >= want && lines[0] != "" {
				return lines
			}
		} else if !os.IsNotExist(err) {
			t.Fatalf("read capture file: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	data, _ := os.ReadFile(path)
	t.Fatalf("timed out waiting for %d captured lines in %s; got %q", want, path, string(data))
	return nil
}

func TestNewSessionWithMock(t *testing.T) {
	// NewSession passes CLI flags (--input-format, --output-format, --verbose)
	// that real `cat` rejects. Use a bash one-liner that ignores args and echoes.
	ctx := context.Background()
	eventCh := make(chan provider.ProviderEvent, 100)
	s, err := NewSession(ctx, testThread, Config{
		Binary: "bash",
		Model:  "", // keep args minimal
	}, func(evt provider.ProviderEvent) {
		eventCh <- evt
	})

	// NewSession spawns bash with args: --input-format stream-json --output-format stream-json --verbose
	// bash doesn't understand these either. Use a different approach:
	// override the binary to a script that ignores args.
	if err != nil {
		// Expected: bash doesn't understand Claude CLI flags.
		// Test NewSession more directly via the helper instead.
		t.Skipf("NewSession with bash fails as expected: %v", err)
	}
	defer s.Close()

	if s.threadID != testThread {
		t.Errorf("threadID: got %q, want %q", s.threadID, testThread)
	}
}

func TestNewSessionSpawnsAndRunsReadLoop(t *testing.T) {
	// Create a script that ignores args and acts like cat.
	scriptDir := t.TempDir()
	scriptPath := scriptDir + "/mock-claude"
	if err := os.WriteFile(scriptPath, []byte("#!/bin/bash\nexec cat\n"), 0755); err != nil {
		t.Fatalf("write mock script: %v", err)
	}

	ctx := context.Background()
	eventCh := make(chan provider.ProviderEvent, 100)
	s, err := NewSession(ctx, testThread, Config{Binary: scriptPath}, func(evt provider.ProviderEvent) {
		eventCh <- evt
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer s.Close()

	if s.threadID != testThread {
		t.Errorf("threadID: got %q, want %q", s.threadID, testThread)
	}
	if s.proc == nil {
		t.Fatal("proc is nil")
	}

	// readLoop should be running — verify by writing an init event.
	initLine := []byte(`{"type":"system","subtype":"init","session_id":"cat-sess","model":"opus","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}`)
	if err := s.proc.WriteLine(initLine); err != nil {
		t.Fatalf("write: %v", err)
	}

	evt := waitEvent(t, eventCh)
	if evt.Kind != provider.EventInit {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventInit)
	}
	if s.SessionID() != "cat-sess" {
		t.Errorf("sessionID: got %q, want %q", s.SessionID(), "cat-sess")
	}
}

func TestSessionSend(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	capturePath := filepath.Join(t.TempDir(), "stdin.ndjson")
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "bash",
		Args:   []string{"-c", `while IFS= read -r line; do printf '%s\n' "$line" >> "$CAPTURE"; exit 0; done`},
		Env: map[string]string{
			"CAPTURE": capturePath,
		},
	})
	if err != nil {
		t.Fatalf("spawn capture process: %v", err)
	}
	t.Cleanup(func() { _ = proc.Close() })

	s := &Session{proc: proc}
	const content = "/tmp/agent-overflow/bug-report.jsonl -- please inspect"
	if err := s.Send(context.Background(), content, provider.SendOptions{}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	lines := waitCapturedLines(t, capturePath, 1)
	var captured struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &captured); err != nil {
		t.Fatalf("unmarshal captured line: %v", err)
	}
	if captured.Type != "user" || captured.Message.Role != "user" {
		t.Fatalf("captured line = %+v, want user message", captured)
	}
	if len(captured.Message.Content) != 1 {
		t.Fatalf("content blocks: got %d, want 1 (%+v)", len(captured.Message.Content), captured.Message.Content)
	}
	if block := captured.Message.Content[0]; block.Type != "text" || block.Text != content {
		t.Fatalf("content block = %+v, want exact text block %q", block, content)
	}
}

// TestSessionSendStampsUserMessageUUID verifies that a client-supplied
// UserMessageUUID rides the user envelope as the top-level `uuid` — the
// contract app_send.go depends on so a revert can slice the transcript
// by a uuid it knew at send time (see the Send doc comment).
func TestSessionSendStampsUserMessageUUID(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	capturePath := filepath.Join(t.TempDir(), "stdin.ndjson")
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "bash",
		Args:   []string{"-c", `while IFS= read -r line; do printf '%s\n' "$line" >> "$CAPTURE"; exit 0; done`},
		Env:    map[string]string{"CAPTURE": capturePath},
	})
	if err != nil {
		t.Fatalf("spawn capture process: %v", err)
	}
	t.Cleanup(func() { _ = proc.Close() })

	const wantUUID = "f47ac10b-58cc-4372-a567-0e02b2c3d479"
	s := &Session{proc: proc}
	if err := s.Send(context.Background(), "hello", provider.SendOptions{UserMessageUUID: wantUUID}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	lines := waitCapturedLines(t, capturePath, 1)
	var captured struct {
		Type string `json:"type"`
		UUID string `json:"uuid"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &captured); err != nil {
		t.Fatalf("unmarshal captured line: %v", err)
	}
	if captured.Type != "user" {
		t.Fatalf("captured type = %q, want user", captured.Type)
	}
	if captured.UUID != wantUUID {
		t.Fatalf("captured top-level uuid = %q, want %q", captured.UUID, wantUUID)
	}
}

// TestSessionSendOmitsUUIDWhenEmpty verifies the optional-field contract:
// when no UserMessageUUID is supplied, the envelope carries no `uuid` key
// and the CLI assigns its own id (legacy behaviour, learned from the echo).
func TestSessionSendOmitsUUIDWhenEmpty(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	capturePath := filepath.Join(t.TempDir(), "stdin.ndjson")
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "bash",
		Args:   []string{"-c", `while IFS= read -r line; do printf '%s\n' "$line" >> "$CAPTURE"; exit 0; done`},
		Env:    map[string]string{"CAPTURE": capturePath},
	})
	if err != nil {
		t.Fatalf("spawn capture process: %v", err)
	}
	t.Cleanup(func() { _ = proc.Close() })

	s := &Session{proc: proc}
	if err := s.Send(context.Background(), "hello", provider.SendOptions{}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	lines := waitCapturedLines(t, capturePath, 1)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(lines[0]), &raw); err != nil {
		t.Fatalf("unmarshal captured line: %v", err)
	}
	if _, present := raw["uuid"]; present {
		t.Fatalf("envelope carried a uuid key with no UserMessageUUID supplied: %s", lines[0])
	}
}

// TestSessionSendRejectsMalformedUUID verifies the validation guard fails
// the send loudly BEFORE any stdin write, so a malformed id never poisons
// the session JSONL with a uuid the revert path can't match.
func TestSessionSendRejectsMalformedUUID(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	capturePath := filepath.Join(t.TempDir(), "stdin.ndjson")
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "bash",
		Args:   []string{"-c", `while IFS= read -r line; do printf '%s\n' "$line" >> "$CAPTURE"; exit 0; done`},
		Env:    map[string]string{"CAPTURE": capturePath},
	})
	if err != nil {
		t.Fatalf("spawn capture process: %v", err)
	}
	t.Cleanup(func() { _ = proc.Close() })

	s := &Session{proc: proc}
	err = s.Send(context.Background(), "hello", provider.SendOptions{UserMessageUUID: "not-a-uuid"})
	if err == nil {
		t.Fatal("Send with malformed uuid returned nil, want error")
	}
	if !strings.Contains(err.Error(), "invalid user message uuid") {
		t.Fatalf("Send error = %v, want invalid-user-message-uuid", err)
	}
	if data, readErr := os.ReadFile(capturePath); readErr == nil && len(data) > 0 {
		t.Fatalf("malformed-uuid send wrote to stdin before validation: %q", data)
	}
}

// TestSessionSendRejectsNonCanonicalUUID verifies that a parseable but
// non-canonical id (here, uppercase) is refused rather than silently
// normalized. app_send.go stamps the exact minted string on the user row
// and checkpoint; if Send canonicalized a different string into the
// envelope the pre-stamped row would no longer match the echoed JSONL
// uuid, dropping a pre-echo revert back to the ordinal-walk fallback. The
// guard makes that contract enforceable at the boundary.
func TestSessionSendRejectsNonCanonicalUUID(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	capturePath := filepath.Join(t.TempDir(), "stdin.ndjson")
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "bash",
		Args:   []string{"-c", `while IFS= read -r line; do printf '%s\n' "$line" >> "$CAPTURE"; exit 0; done`},
		Env:    map[string]string{"CAPTURE": capturePath},
	})
	if err != nil {
		t.Fatalf("spawn capture process: %v", err)
	}
	t.Cleanup(func() { _ = proc.Close() })

	s := &Session{proc: proc}
	// Uppercase of a valid uuid: uuid.Parse accepts it, but String()
	// round-trips to the lowercase canonical form, so the guard rejects it.
	err = s.Send(context.Background(), "hello", provider.SendOptions{UserMessageUUID: "F47AC10B-58CC-4372-A567-0E02B2C3D479"})
	if err == nil {
		t.Fatal("Send with non-canonical uuid returned nil, want error")
	}
	if !strings.Contains(err.Error(), "not in canonical form") {
		t.Fatalf("Send error = %v, want not-in-canonical-form", err)
	}
	if data, readErr := os.ReadFile(capturePath); readErr == nil && len(data) > 0 {
		t.Fatalf("non-canonical-uuid send wrote to stdin before validation: %q", data)
	}
}

func TestSessionSendSetsPlanPermissionModeBeforeUserMessage(t *testing.T) {
	scriptPath := filepath.Join(t.TempDir(), "fake-claude")
	capturePath := filepath.Join(t.TempDir(), "stdin.ndjson")
	script := `#!/bin/sh
set -eu
capture="${CAPTURE_FILE:?}"
while IFS= read -r line; do
    printf '%s\n' "$line" >> "$capture"
    case "$line" in
        *'"set_permission_mode"'*)
            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"
            ;;
        *'"type":"user"'*)
            exit 0
            ;;
    esac
done
`
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write fake claude script: %v", err)
	}

	s, err := NewSession(context.Background(), testThread, Config{
		Binary:             scriptPath,
		BasePermissionMode: "default",
		InteractionMode:    provider.ModePlan,
		Env: map[string]string{
			"CAPTURE_FILE": capturePath,
		},
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer s.Close()
	s.controlRequestTimeout = 2 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.Send(ctx, "draft a plan", provider.SendOptions{}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	lines := waitCapturedLines(t, capturePath, 2)
	var first struct {
		Type    string `json:"type"`
		Request struct {
			Subtype string `json:"subtype"`
			Mode    string `json:"mode"`
		} `json:"request"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("unmarshal first captured line: %v", err)
	}
	if first.Type != "control_request" || first.Request.Subtype != "set_permission_mode" || first.Request.Mode != "plan" {
		t.Fatalf("first captured line = %+v, want set_permission_mode plan", first)
	}

	var second struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("unmarshal second captured line: %v", err)
	}
	if second.Type != "user" || second.Message.Role != "user" || len(second.Message.Content) != 1 || second.Message.Content[0].Type != "text" || second.Message.Content[0].Text != "draft a plan" {
		t.Fatalf("second captured line = %+v, want user message", second)
	}
	if got := s.getCurrentPermissionMode(); got != "plan" {
		t.Fatalf("currentPermissionMode = %q, want plan", got)
	}
}

func TestSessionSendRestoresBasePermissionModeAfterPlanTurn(t *testing.T) {
	for _, baseMode := range []string{"default", "acceptEdits", "bypassPermissions"} {
		t.Run(baseMode, func(t *testing.T) {
			scriptPath := filepath.Join(t.TempDir(), "fake-claude")
			capturePath := filepath.Join(t.TempDir(), "stdin.ndjson")
			script := `#!/bin/sh
set -eu
capture="${CAPTURE_FILE:?}"
users=0
while IFS= read -r line; do
    printf '%s\n' "$line" >> "$capture"
    case "$line" in
        *'"set_permission_mode"'*)
            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"
            ;;
        *'"type":"user"'*)
            users=$((users + 1))
            if [ "$users" -ge 2 ]; then
                exit 0
            fi
            ;;
    esac
done
`
			if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
				t.Fatalf("write fake claude script: %v", err)
			}

			s, err := NewSession(context.Background(), testThread, Config{
				Binary:             scriptPath,
				BasePermissionMode: baseMode,
				InteractionMode:    provider.ModeChat,
				Env: map[string]string{
					"CAPTURE_FILE": capturePath,
				},
			}, func(provider.ProviderEvent) {})
			if err != nil {
				t.Fatalf("NewSession: %v", err)
			}
			defer s.Close()
			s.controlRequestTimeout = 2 * time.Second

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := s.Send(ctx, "draft a plan", provider.SendOptions{InteractionMode: provider.ModePlan}); err != nil {
				t.Fatalf("plan Send: %v", err)
			}
			if err := s.Send(ctx, "implement it", provider.SendOptions{InteractionMode: provider.ModeChat}); err != nil {
				t.Fatalf("chat Send: %v", err)
			}

			lines := waitCapturedLines(t, capturePath, 4)
			var modes []string
			for _, line := range lines {
				var raw struct {
					Type    string `json:"type"`
					Request struct {
						Subtype string `json:"subtype"`
						Mode    string `json:"mode"`
					} `json:"request"`
				}
				if err := json.Unmarshal([]byte(line), &raw); err != nil {
					t.Fatalf("unmarshal captured line %q: %v", line, err)
				}
				if raw.Type == "control_request" && raw.Request.Subtype == "set_permission_mode" {
					modes = append(modes, raw.Request.Mode)
				}
			}
			if want := []string{"plan", baseMode}; !reflect.DeepEqual(modes, want) {
				t.Fatalf("set_permission_mode sequence = %v, want %v", modes, want)
			}
			if got := s.getCurrentPermissionMode(); got != baseMode {
				t.Fatalf("currentPermissionMode = %q, want %q", got, baseMode)
			}
		})
	}
}

func TestFullAccessToolRequestDoesNotAutoApproveInPlanMode(t *testing.T) {
	s := &Session{
		basePermissionMode:    "bypassPermissions",
		currentPermissionMode: "plan",
		interactionMode:       provider.ModePlan,
	}
	handled, err := s.maybeHandleFullAccessToolRequest([]byte(`{"type":"control_request","request_id":"req-1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`))
	if err != nil {
		t.Fatalf("maybeHandleFullAccessToolRequest: %v", err)
	}
	if handled {
		t.Fatal("plan-mode session auto-approved full-access tool request")
	}
}

func TestFullAccessToolRequestAutoApprovesRegularTools(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	capturePath := filepath.Join(t.TempDir(), "stdin.ndjson")
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "bash",
		Args:   []string{"-c", `while IFS= read -r line; do printf '%s\n' "$line" >> "$CAPTURE"; done`},
		Env: map[string]string{
			"CAPTURE": capturePath,
		},
	})
	if err != nil {
		t.Fatalf("spawn capture process: %v", err)
	}
	defer cancel()
	t.Cleanup(func() { _ = proc.Close() })

	s := &Session{
		proc:                  proc,
		currentPermissionMode: "bypassPermissions",
	}
	handled, err := s.maybeHandleFullAccessToolRequest([]byte(`{"type":"control_request","request_id":"req-1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`))
	if err != nil {
		t.Fatalf("maybeHandleFullAccessToolRequest: %v", err)
	}
	if !handled {
		t.Fatal("full-access regular tool request was not auto-approved")
	}
	lines := waitCapturedLines(t, capturePath, 1)
	var response struct {
		Type     string `json:"type"`
		Response struct {
			Subtype   string `json:"subtype"`
			RequestID string `json:"request_id"`
			Response  struct {
				Behavior string `json:"behavior"`
			} `json:"response"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &response); err != nil {
		t.Fatalf("unmarshal auto-approval response: %v", err)
	}
	if response.Type != "control_response" ||
		response.Response.Subtype != "success" ||
		response.Response.RequestID != "req-1" ||
		response.Response.Response.Behavior != "allow" {
		t.Fatalf("auto-approval response = %+v, want allow for req-1", response)
	}
}

func TestFullAccessToolRequestLeavesInteractiveExceptionsPending(t *testing.T) {
	for _, toolName := range []string{"AskUserQuestion", "ExitPlanMode"} {
		t.Run(toolName, func(t *testing.T) {
			s := &Session{currentPermissionMode: "bypassPermissions"}
			line := []byte(`{"type":"control_request","request_id":"req-1","request":{"subtype":"can_use_tool","tool_name":"` + toolName + `"}}`)
			handled, err := s.maybeHandleFullAccessToolRequest(line)
			if err != nil {
				t.Fatalf("maybeHandleFullAccessToolRequest: %v", err)
			}
			if handled {
				t.Fatalf("%s should remain interactive in full-access mode", toolName)
			}
		})
	}
}

// interruptResponderScript is a bash fake-CLI that reads stdin line by
// line and writes a canned control_response for every interrupt
// request it sees. Mirrors stopTaskResponderScript:
//   - "success": subtype=success, echoes back the request_id
//   - "error":   subtype=error with a provider-side message
//   - "silent":  drops the line; never responds (timeout/kill path)
func interruptResponderScript(mode string) string {
	// The case alternation accepts either field order because
	// json.Marshal on a map[string]any sorts keys alphabetically — the
	// "type" field can land either before or after "subtype" depending
	// on what other keys are present.
	const header = `#!/bin/sh
set -u
while IFS= read -r line; do
    case "$line" in
        *'"type":"control_request"'*'"subtype":"interrupt"'* | *'"subtype":"interrupt"'*'"type":"control_request"'*)
            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
`
	const footer = `
            ;;
    esac
done
`
	var body string
	switch mode {
	case "success":
		body = `            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"`
	case "error":
		body = `            printf '{"type":"control_response","response":{"subtype":"error","request_id":"%s","error":"no active turn"}}\n' "$reqid"`
	case "silent":
		body = `            : # drop the line deliberately to exercise the timeout fallback`
	default:
		body = `            : # unknown mode — never happens in tests`
	}
	return header + body + footer
}

// newInterruptResponderSession spawns a Session backed by the fake-CLI
// script returned by interruptResponderScript. Wraps the boilerplate
// shared by the Interrupt round-trip tests, mirroring
// newStopTaskResponderSession.
func newInterruptResponderSession(t *testing.T, mode string, interruptTimeout time.Duration) *Session {
	t.Helper()
	scriptDir := t.TempDir()
	scriptPath := scriptDir + "/fake-claude"
	if err := os.WriteFile(scriptPath, []byte(interruptResponderScript(mode)), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: scriptPath})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	s := &Session{
		proc:                  proc,
		threadID:              testThread,
		onEvent:               func(evt provider.ProviderEvent) { _ = evt },
		cancel:                cancel,
		readDone:              make(chan struct{}),
		controlRequestTimeout: interruptTimeout,
	}
	go s.readLoop()
	t.Cleanup(func() {
		_ = s.Close()
	})
	return s
}

func TestSessionRespondToApproval(t *testing.T) {
	s, _ := newTestClaudeSession(t)

	// Each sub-case uses a distinct request ID: Bug B9 dedup rejects
	// repeat responses for the same ID, so reusing "req-1" across all
	// three iterations would trip ErrApprovalAlreadyResolved on the
	// second decision. Unique IDs keep the test focused on the decision
	// encoding, which is what it is supposed to cover.
	decisions := []string{"allow", "deny", "allow_session"}
	for _, d := range decisions {
		t.Run(d, func(t *testing.T) {
			s.trackPendingApproval("req-"+d, provider.EventApprovalResolved)
			err := s.RespondToApproval(context.Background(), provider.ApprovalResponse{
				RequestID: "req-" + d,
				Decision:  d,
			})
			if err != nil {
				t.Fatalf("RespondToApproval(%s): %v", d, err)
			}
		})
	}
}

func TestSessionRespondToUserInputIncludesQuestionsInUpdatedInput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	capturePath := filepath.Join(t.TempDir(), "stdin.ndjson")
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "bash",
		Args:   []string{"-c", `while IFS= read -r line; do printf '%s\n' "$line" >> "$CAPTURE"; done`},
		Env: map[string]string{
			"CAPTURE": capturePath,
		},
	})
	if err != nil {
		t.Fatalf("spawn capture process: %v", err)
	}
	s := &Session{
		proc:     proc,
		threadID: testThread,
		onEvent:  func(provider.ProviderEvent) {},
		cancel:   cancel,
	}
	t.Cleanup(func() {
		cancel()
		proc.Close()
	})

	questions := []provider.UserInputQuestion{{
		ID:       "framework",
		Header:   "Framework",
		Question: "Pick one",
		Options: []provider.UserInputQuestionOption{{
			Label:       "Svelte",
			Description: "Use Svelte",
		}},
	}, {
		ID:          "extras",
		Header:      "Extras",
		Question:    "Pick extras",
		MultiSelect: true,
		Options: []provider.UserInputQuestionOption{{
			Label:       "lint",
			Description: "Run lint",
		}, {
			Label:       "tests",
			Description: "Run tests",
		}},
	}}
	s.trackPendingApprovalWithQuestions("req-user-input", provider.EventUserInputResolved, questions)

	err = s.RespondToUserInput(context.Background(), provider.UserInputResponse{
		RequestID: "req-user-input",
		Decision:  "accept",
		Answers: map[string]provider.UserInputAnswer{
			"framework": provider.SingleUserInputAnswer("Svelte"),
			"extras":    provider.UserInputAnswer{"lint", "tests"},
		},
	})
	if err != nil {
		t.Fatalf("RespondToUserInput: %v", err)
	}

	var captured []byte
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		captured, err = os.ReadFile(capturePath)
		if err == nil && len(captured) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(captured) == 0 {
		t.Fatalf("capture file was empty: %v", err)
	}

	var msg struct {
		Response struct {
			Response struct {
				Behavior     string `json:"behavior"`
				UpdatedInput struct {
					Answers   map[string]string            `json:"answers"`
					Questions []provider.UserInputQuestion `json:"questions"`
				} `json:"updatedInput"`
			} `json:"response"`
		} `json:"response"`
	}
	if err := json.Unmarshal(captured, &msg); err != nil {
		t.Fatalf("unmarshal captured response: %v (data=%s)", err, captured)
	}
	if msg.Response.Response.Behavior != "allow" {
		t.Fatalf("behavior = %q, want allow", msg.Response.Response.Behavior)
	}
	if msg.Response.Response.UpdatedInput.Answers["Pick one"] != "Svelte" {
		t.Fatalf("answers = %+v, want Pick one=Svelte", msg.Response.Response.UpdatedInput.Answers)
	}
	if msg.Response.Response.UpdatedInput.Answers["Pick extras"] != "lint, tests" {
		t.Fatalf("answers = %+v, want Pick extras=\"lint, tests\"", msg.Response.Response.UpdatedInput.Answers)
	}
	if !reflect.DeepEqual(msg.Response.Response.UpdatedInput.Questions, questions) {
		t.Fatalf("questions = %+v, want %+v", msg.Response.Response.UpdatedInput.Questions, questions)
	}
}

func TestClaudeAskUserQuestionAnswersAvoidsDuplicateQuestionTextCollision(t *testing.T) {
	questions := []provider.UserInputQuestion{{
		ID:       "first",
		Header:   "First choice",
		Question: "Pick one",
	}, {
		ID:       "second",
		Header:   "Second choice",
		Question: "Pick one",
	}}

	got := claudeAskUserQuestionAnswers(questions, map[string]provider.UserInputAnswer{
		"first":  provider.SingleUserInputAnswer("React"),
		"second": provider.SingleUserInputAnswer("Svelte"),
	})

	if got["First choice"] != "React" {
		t.Fatalf("first answer = %+v, want First choice=React", got)
	}
	if got["Second choice"] != "Svelte" {
		t.Fatalf("second answer = %+v, want Second choice=Svelte", got)
	}
	if _, ok := got["Pick one"]; ok {
		t.Fatalf("duplicate question text key was used: %+v", got)
	}
}

func TestSessionIDAccessor(t *testing.T) {
	s, eventCh := newTestClaudeSession(t)

	// Before init, session ID should be empty.
	if s.SessionID() != "" {
		t.Errorf("SessionID should be empty before init, got %q", s.SessionID())
	}

	// Write init to set it.
	initLine := []byte(`{"type":"system","subtype":"init","session_id":"test-sid","model":"opus","cwd":"/","tools":[],"claude_code_version":"1.0"}`)
	if err := s.proc.WriteLine(initLine); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitEvent(t, eventCh) // wait for init event to be processed

	if s.SessionID() != "test-sid" {
		t.Errorf("SessionID: got %q, want %q", s.SessionID(), "test-sid")
	}
}

func TestReadLoopDispatchesTextDelta(t *testing.T) {
	s, eventCh := newTestClaudeSession(t)

	// Text content reaches the read loop via stream_event envelopes
	// (the CLI always runs with --include-partial-messages). The
	// coalesced `assistant` envelope's text blocks are intentionally
	// skipped by the parser to avoid doubling the summary.
	line := []byte(`{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":"text_delta","text":"streaming text"}}}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}

	evt := waitEvent(t, eventCh)
	if evt.Kind != provider.EventTextDelta {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventTextDelta)
	}
	if evt.Content != "streaming text" {
		t.Errorf("content: got %q, want %q", evt.Content, "streaming text")
	}
}

func TestReadLoopDispatchesTurnComplete(t *testing.T) {
	s, eventCh := newTestClaudeSession(t)

	line := []byte(`{"type":"result","subtype":"success","is_error":false,"result":"Done","session_id":"s1"}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}

	evt := waitEvent(t, eventCh)
	if evt.Kind != provider.EventTurnComplete {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventTurnComplete)
	}
}

func TestReadLoopContinuesOnParseError(t *testing.T) {
	s, eventCh := newTestClaudeSession(t)

	// Write invalid JSON — readLoop should log and continue.
	if err := s.proc.WriteLine([]byte(`not valid json at all`)); err != nil {
		t.Fatalf("write bad line: %v", err)
	}

	// Write valid event after — readLoop should still be running.
	if err := s.proc.WriteLine([]byte(`{"type":"result","subtype":"success","is_error":false}`)); err != nil {
		t.Fatalf("write good line: %v", err)
	}

	evt := waitEvent(t, eventCh)
	if evt.Kind != provider.EventTurnComplete {
		t.Errorf("expected turn_complete after parse error recovery, got %q", evt.Kind)
	}
}

func TestReadLoopEmitsDisconnectedOnExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: "cat"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	eventCh := make(chan provider.ProviderEvent, 100)
	s := &Session{
		proc:     proc,
		threadID: testThread,
		onEvent: func(evt provider.ProviderEvent) {
			eventCh <- evt
		},
		cancel:   cancel,
		readDone: make(chan struct{}),
	}
	go s.readLoop()

	// Close the session — should emit disconnected.
	s.Close()

	var gotDisconnected bool
	timeout := time.After(5 * time.Second)
	for !gotDisconnected {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventSessionStatus && evt.Content == "disconnected" {
				gotDisconnected = true
			}
		case <-timeout:
			t.Fatal("timeout waiting for disconnected event")
		}
	}
}

func TestCloseWaitsForDisconnectedHandler(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: "cat"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	disconnected := make(chan struct{})
	release := make(chan struct{})
	closeReturned := make(chan struct{})
	s := &Session{
		proc:     proc,
		threadID: testThread,
		onEvent: func(evt provider.ProviderEvent) {
			if evt.Kind == provider.EventSessionStatus && evt.Content == "disconnected" {
				close(disconnected)
				<-release
			}
		},
		cancel:   cancel,
		readDone: make(chan struct{}),
	}
	go s.readLoop()

	go func() {
		_ = s.Close()
		close(closeReturned)
	}()

	select {
	case <-disconnected:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for disconnected handler")
	}

	select {
	case <-closeReturned:
		t.Fatal("Close returned before disconnected handler completed")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)

	select {
	case <-closeReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for Close to return")
	}
}

func TestClaudeApprovalWaitsForUserResponseWithoutTimeout(t *testing.T) {
	s, eventCh := newTestClaudeSessionWithPendingRequests(t)

	approvalLine := []byte(`{"type":"control_request","request_id":"req-waiting","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`)
	if err := s.proc.WriteLine(approvalLine); err != nil {
		t.Fatalf("write approval: %v", err)
	}

	for {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventApprovalRequest {
				goto waitWithoutResolution
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no approval event")
		}
	}

waitWithoutResolution:
	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case evt := <-eventCh:
			switch evt.Kind {
			case provider.EventError, provider.EventApprovalResolved:
				t.Fatalf("pending approval resolved without user action: %+v", evt)
			}
		case <-deadline:
			if err := s.RespondToApproval(context.Background(), provider.ApprovalResponse{
				RequestID: "req-waiting",
				Decision:  "allow",
			}); err != nil {
				t.Fatalf("RespondToApproval after waiting: %v", err)
			}
			return
		}
	}
}

func TestClaudeUserInputWaitsForUserResponseWithoutTimeout(t *testing.T) {
	s, eventCh := newTestClaudeSessionWithPendingRequests(t)

	uqLine := []byte(`{"type":"control_request","request_id":"uq-waiting","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":[{"id":"scope","header":"Scope","question":"Choose","options":[{"label":"turn","description":"This turn"}]}]}}}`)
	if err := s.proc.WriteLine(uqLine); err != nil {
		t.Fatalf("write user-input request: %v", err)
	}

	for {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventUserInputRequest {
				goto waitWithoutResolution
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no user-input request event")
		}
	}

waitWithoutResolution:
	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case evt := <-eventCh:
			switch evt.Kind {
			case provider.EventError, provider.EventUserInputResolved, provider.EventApprovalResolved:
				t.Fatalf("pending user input resolved without user action: %+v", evt)
			}
		case <-deadline:
			err := s.RespondToUserInput(context.Background(), provider.UserInputResponse{
				RequestID: "uq-waiting",
				Decision:  "accept",
				Answers: map[string]provider.UserInputAnswer{
					"scope": provider.SingleUserInputAnswer("turn"),
				},
			})
			if err != nil {
				t.Fatalf("RespondToUserInput after waiting: %v", err)
			}
			return
		}
	}
}

func TestApprovalResponseResolvesPendingClaude(t *testing.T) {
	s, eventCh := newTestClaudeSessionWithPendingRequests(t)

	approvalLine := []byte(`{"type":"control_request","request_id":"req-normal","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`)
	if err := s.proc.WriteLine(approvalLine); err != nil {
		t.Fatalf("write approval: %v", err)
	}

	// Wait for the approval event to arrive.
	var gotApproval bool
	for !gotApproval {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventApprovalRequest {
				gotApproval = true
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no approval event")
		}
	}

	if err := s.RespondToApproval(context.Background(), provider.ApprovalResponse{
		RequestID: "req-normal",
		Decision:  "allow",
	}); err != nil {
		t.Fatalf("RespondToApproval: %v", err)
	}

	if err := s.RespondToApproval(context.Background(), provider.ApprovalResponse{
		RequestID: "req-normal",
		Decision:  "deny",
	}); !errors.Is(err, provider.ErrStaleInteractiveRequest) {
		t.Fatalf("second RespondToApproval error = %v, want ErrStaleInteractiveRequest", err)
	}
}

func TestClaudeCloseResolvesPendingApprovalAsLost(t *testing.T) {
	s, eventCh := newTestClaudeSessionWithPendingRequests(t)

	approvalLine := []byte(`{"type":"control_request","request_id":"req-close","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`)
	if err := s.proc.WriteLine(approvalLine); err != nil {
		t.Fatalf("write approval: %v", err)
	}

	var gotApproval bool
	for !gotApproval {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventApprovalRequest {
				gotApproval = true
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no approval event")
		}
	}

	if err := s.Close(); err != nil {
		t.Logf("close returned %v (acceptable)", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case evt, ok := <-eventCh:
			if !ok {
				t.Fatal("event channel closed before approval resolved")
			}
			if evt.Kind != provider.EventApprovalResolved {
				continue
			}
			var meta map[string]any
			if err := json.Unmarshal(evt.Meta, &meta); err != nil {
				t.Fatalf("unmarshal resolved meta: %v", err)
			}
			if meta["decision"] != "lost" {
				t.Fatalf("decision = %v, want lost", meta["decision"])
			}
			return
		case <-deadline:
			t.Fatal("pending approval was not resolved on close")
		}
	}
}

func TestClaudeProviderExitResolvesPendingUserInputAsLost(t *testing.T) {
	s, eventCh := newTestClaudeSessionWithPendingRequests(t)

	uqLine := []byte(`{"type":"control_request","request_id":"uq-exit","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":[{"id":"scope","header":"Scope","question":"Choose","options":[{"label":"turn","description":"This turn"}]}]}}}`)
	if err := s.proc.WriteLine(uqLine); err != nil {
		t.Fatalf("write user-input request: %v", err)
	}

	for {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventUserInputRequest {
				goto closeProvider
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no user-input request event")
		}
	}

closeProvider:
	if err := s.proc.Close(); err != nil {
		t.Fatalf("close provider process: %v", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case evt := <-eventCh:
			if evt.Kind != provider.EventUserInputResolved {
				continue
			}
			var meta map[string]any
			if err := json.Unmarshal(evt.Meta, &meta); err != nil {
				t.Fatalf("unmarshal resolved meta: %v", err)
			}
			if meta["decision"] != "lost" {
				t.Fatalf("decision = %v, want lost", meta["decision"])
			}
			if _, ok := meta["answers"].(map[string]any); !ok {
				t.Fatalf("answers missing or wrong type: %v", meta["answers"])
			}
			return
		case <-deadline:
			t.Fatal("pending user input was not resolved after provider exit")
		}
	}
}

func TestClaudeCloseResolvesPendingUserInputAsLost(t *testing.T) {
	s, eventCh := newTestClaudeSessionWithPendingRequests(t)

	uqLine := []byte(`{"type":"control_request","request_id":"uq-close","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":[{"id":"scope","header":"Scope","question":"Choose","options":[{"label":"turn","description":"This turn"}]}]}}}`)
	if err := s.proc.WriteLine(uqLine); err != nil {
		t.Fatalf("write user-input request: %v", err)
	}

	for {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventUserInputRequest {
				goto closeSession
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no user-input request event")
		}
	}

closeSession:
	if err := s.Close(); err != nil {
		t.Logf("close returned %v (acceptable)", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case evt := <-eventCh:
			if evt.Kind != provider.EventUserInputResolved {
				continue
			}
			var meta map[string]any
			if err := json.Unmarshal(evt.Meta, &meta); err != nil {
				t.Fatalf("unmarshal resolved meta: %v", err)
			}
			if meta["decision"] != "lost" {
				t.Fatalf("decision = %v, want lost", meta["decision"])
			}
			if _, ok := meta["answers"].(map[string]any); !ok {
				t.Fatalf("answers missing or wrong type: %v", meta["answers"])
			}
			err := s.RespondToUserInput(context.Background(), provider.UserInputResponse{
				RequestID: "uq-close",
				Decision:  "accept",
				Answers: map[string]provider.UserInputAnswer{
					"scope": provider.SingleUserInputAnswer("turn"),
				},
			})
			if !errors.Is(err, provider.ErrStaleInteractiveRequest) {
				t.Fatalf("RespondToUserInput after close error = %v, want ErrStaleInteractiveRequest", err)
			}
			return
		case <-deadline:
			t.Fatal("pending user input was not resolved on close")
		}
	}
}

// TestControlCancelRequestClearsPendingApproval exercises the
// `control_cancel_request` cleanup path. When an interrupt aborts an
// in-flight can_use_tool callback, the CLI emits this envelope to
// abandon the prior request. We must:
//   - clear the pending approval / user-input state (so the panel
//     disappears),
//   - emit the matching resolved event with cancel semantics,
//   - NOT write a control_response (the CLI is no longer waiting).
//
// Mirror tests for both the approval and user-input flavours. Bug-fix
// tracker: agent-overflow merry-wirth plan, step 3.
func TestControlCancelRequestClearsPendingApproval(t *testing.T) {
	s, eventCh := newTestClaudeSessionWithPendingRequests(t)

	// 1. CLI emits an approval request.
	approvalLine := []byte(`{"type":"control_request","request_id":"req-cancel","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`)
	if err := s.proc.WriteLine(approvalLine); err != nil {
		t.Fatalf("write approval: %v", err)
	}

	var gotApproval bool
	for !gotApproval {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventApprovalRequest && evt.ItemID == "req-cancel" {
				gotApproval = true
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no approval event")
		}
	}

	// 2. CLI later abandons it via control_cancel_request (e.g. user
	// interrupt fired, SDK-side AbortSignal).
	cancelLine := []byte(`{"type":"control_cancel_request","request_id":"req-cancel"}`)
	if err := s.proc.WriteLine(cancelLine); err != nil {
		t.Fatalf("write cancel: %v", err)
	}

	// 3. Expect EventApprovalResolved with decision:"cancel".
	deadline := time.After(2 * time.Second)
	var gotResolved bool
	for !gotResolved {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventApprovalResolved && evt.ItemID == "req-cancel" {
				var meta map[string]any
				if err := json.Unmarshal(evt.Meta, &meta); err != nil {
					t.Fatalf("unmarshal resolved meta: %v", err)
				}
				if meta["decision"] != "cancel" {
					t.Fatalf("resolved decision: got %v, want cancel", meta["decision"])
				}
				gotResolved = true
			}
		case <-deadline:
			t.Fatal("never saw EventApprovalResolved for cancelled request")
		}
	}

	// 4. A subsequent RespondToApproval for the same id must short-
	// circuit: the request is already resolved.
	respErr := s.RespondToApproval(context.Background(), provider.ApprovalResponse{
		RequestID: "req-cancel",
		Decision:  "allow",
	})
	if respErr == nil {
		t.Fatalf("RespondToApproval after cancel: expected error, got nil")
	}
	if !errors.Is(respErr, provider.ErrStaleInteractiveRequest) {
		t.Fatalf("RespondToApproval after cancel: got %v, want ErrStaleInteractiveRequest", respErr)
	}
}

// TestControlCancelRequestClearsPendingUserInput is the AskUserQuestion
// flavour: when the CLI cancels a pending user-input prompt, the
// resolved event must carry empty answers and decision="cancel" so
// the panel above the composer clears.
func TestControlCancelRequestClearsPendingUserInput(t *testing.T) {
	s, eventCh := newTestClaudeSessionWithPendingRequests(t)

	uqLine := []byte(`{"type":"control_request","request_id":"uq-cancel","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":[{"id":"q","header":"Pick","question":"a or b?","options":[{"label":"a","description":"opt a"},{"label":"b","description":"opt b"}]}]}}}`)
	if err := s.proc.WriteLine(uqLine); err != nil {
		t.Fatalf("write user-input request: %v", err)
	}

	var gotRequest bool
	for !gotRequest {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventUserInputRequest && evt.ItemID == "uq-cancel" {
				gotRequest = true
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no user-input request event")
		}
	}

	cancelLine := []byte(`{"type":"control_cancel_request","request_id":"uq-cancel"}`)
	if err := s.proc.WriteLine(cancelLine); err != nil {
		t.Fatalf("write cancel: %v", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventUserInputResolved && evt.ItemID == "uq-cancel" {
				var meta map[string]any
				if err := json.Unmarshal(evt.Meta, &meta); err != nil {
					t.Fatalf("unmarshal resolved meta: %v", err)
				}
				if meta["decision"] != "cancel" {
					t.Fatalf("resolved decision: got %v, want cancel", meta["decision"])
				}
				answers, ok := meta["answers"].(map[string]any)
				if !ok {
					t.Fatalf("answers missing or wrong type: %v", meta["answers"])
				}
				if len(answers) != 0 {
					t.Fatalf("answers: got %v, want empty map", answers)
				}
				return
			}
		case <-deadline:
			t.Fatal("never saw EventUserInputResolved for cancelled request")
		}
	}
}

// newTestClaudeSessionWithPendingRequests wires up a cat-backed session
// for tests that need a live read loop plus pending interactive requests.
func newTestClaudeSessionWithPendingRequests(t *testing.T) (*Session, <-chan provider.ProviderEvent) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: "cat"})
	if err != nil {
		t.Fatalf("spawn cat: %v", err)
	}
	eventCh := make(chan provider.ProviderEvent, 200)
	s := &Session{
		proc:     proc,
		threadID: testThread,
		onEvent: func(evt provider.ProviderEvent) {
			eventCh <- evt
		},
		cancel:   cancel,
		readDone: make(chan struct{}),
	}
	go s.readLoop()
	t.Cleanup(func() {
		cancel()
		proc.Close()
	})
	return s, eventCh
}

// TestExitPlanModeWriteFailureClosesSession exercises Bug B7: when the
// synthetic deny-control_response can't be written (stdin closed, pipe
// broken, subprocess gone), the old readLoop just logged and kept
// going — leaving the subprocess hung waiting for a reply. The fix
// treats the write failure as a session-fatal error: readLoop closes
// the subprocess, emits EventError, and reaches the disconnected
// terminal state.
func TestExitPlanModeWriteFailureClosesSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Close stdin (the read end of our write pipe), then print the plan
	// request while keeping the subprocess alive briefly. The ordering is
	// load-bearing: if the request is printed first, readLoop can race ahead
	// and write the denial before the shell has actually closed fd 0.
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args:   []string{"-c", `exec 0<&-; printf '{"type":"control_request","request_id":"plan-1","request":{"subtype":"can_use_tool","tool_name":"ExitPlanMode","input":{"plan":"# plan"}}}\n'; sleep 0.05`},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer proc.Kill()

	eventCh := make(chan provider.ProviderEvent, 100)
	s := &Session{
		proc:     proc,
		threadID: testThread,
		onEvent: func(evt provider.ProviderEvent) {
			eventCh <- evt
		},
		cancel:   cancel,
		readDone: make(chan struct{}),
	}
	go s.readLoop()

	// Expect: EventProposedPlan fires (read from subprocess line),
	// then the write fails (subprocess already exited), then an
	// EventError describing the failure, then disconnected.
	var gotPlan, gotWriteErr, gotDisconnected bool
	deadline := time.After(5 * time.Second)
	for !(gotPlan && gotWriteErr && gotDisconnected) {
		select {
		case evt := <-eventCh:
			switch {
			case evt.Kind == provider.EventProposedPlan:
				gotPlan = true
			case evt.Kind == provider.EventError &&
				(strings.Contains(evt.Content, "exit plan mode") || strings.Contains(evt.Content, "plan mode response")):
				gotWriteErr = true
			case evt.Kind == provider.EventSessionStatus && evt.Content == "disconnected":
				gotDisconnected = true
			}
		case <-deadline:
			t.Fatalf("timeout (plan=%v writeErr=%v disc=%v)", gotPlan, gotWriteErr, gotDisconnected)
		}
	}
}

// TestExitPlanModeWritesDenyOnHappyPath verifies the normal path is
// unchanged: a plan arrives, the deny response is written, the
// subprocess continues happily.
func TestExitPlanModeWritesDenyOnHappyPath(t *testing.T) {
	s, eventCh := newTestClaudeSession(t)

	planReq := []byte(`{"type":"control_request","request_id":"plan-ok","request":{"subtype":"can_use_tool","tool_name":"ExitPlanMode","input":{"plan":"# hi"}}}`)
	if err := s.proc.WriteLine(planReq); err != nil {
		t.Fatalf("write plan request: %v", err)
	}

	var sawPlan bool
	for !sawPlan {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventProposedPlan {
				sawPlan = true
			}
		case <-time.After(3 * time.Second):
			t.Fatal("never saw EventProposedPlan")
		}
	}

	// The subprocess (cat) echoes the deny response back. readLoop
	// will parse the echoed line — it should be a control_response,
	// which ParseLine returns 0 events for, and readLoop continues.
	// Fire another normal line to confirm readLoop survives.
	if err := s.proc.WriteLine([]byte(`{"type":"system","subtype":"future_feature"}`)); err != nil {
		t.Fatalf("write follow-up: %v", err)
	}

	// Confirm no EventError arrives.
	select {
	case evt := <-eventCh:
		if evt.Kind == provider.EventError {
			t.Fatalf("unexpected error after happy-path plan mode: %v", evt.Content)
		}
	case <-time.After(200 * time.Millisecond):
		// ok
	}
}

// (TestIdleWatchdog* deleted; the per-turn idle watchdog was removed
// because it incorrectly killed the subprocess while waiting for a
// pending can_use_tool / AskUserQuestion response. See plan: t3-code
// has no equivalent watchdog and the user-facing Stop button is the
// authoritative way to abort a stuck turn.)

// TestReadLoopEmitsErrorOnOversizedLine exercises Bug B1 at the readLoop
// layer: when the subprocess writes a single line past the cap, we expect
// (1) an EventError describing the overflow, (2) the session to reach the
// disconnected terminal state, and (3) the subprocess to be reaped (no
// orphan). A regression that swallowed the error — the pre-fix behaviour —
// would leave readLoop exiting silently while the subprocess kept running.
func TestReadLoopEmitsErrorOnOversizedLine(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	script := "perl -e 'print \"x\" x (33 * 1024 * 1024)'; sleep 30"
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args:   []string{"-c", script},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	eventCh := make(chan provider.ProviderEvent, 100)
	s := &Session{
		proc:     proc,
		threadID: testThread,
		onEvent: func(evt provider.ProviderEvent) {
			eventCh <- evt
		},
		cancel:   cancel,
		readDone: make(chan struct{}),
	}
	go s.readLoop()

	var gotOverflowError, gotDisconnected bool
	timeout := time.After(15 * time.Second)
	for !(gotOverflowError && gotDisconnected) {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventError && containsAny(evt.Content, "exceeded maximum size", "cap=") {
				gotOverflowError = true
			}
			if evt.Kind == provider.EventSessionStatus && evt.Content == "disconnected" {
				gotDisconnected = true
			}
		case <-timeout:
			t.Fatalf("timeout waiting for oversize error + disconnected (got overflow=%v disconnected=%v)", gotOverflowError, gotDisconnected)
		}
	}

	// Process must be reaped — the orphan-process bug would leave it alive.
	select {
	case <-proc.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("process not reaped after oversized line (B1 regression)")
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// TestReadLoopEmitsErrorStatusOnCleanUnexpectedExit pins the
// quiet-disconnect bug fix: a subprocess that exits with status 0
// while we still expected it to be running is just as much an
// abnormal exit as one that returned a non-zero code. The previous
// gate (`if exitErr != nil`) skipped the "error" event whenever
// WaitProcessExitErr returned nil — either because the process
// exited cleanly (exit code 0) or because the 100ms wait timed out
// before the OS reaped the child. Without the "error" event, triage's
// handleSessionDied never ran, so the FE working indicator stayed
// stranded until the user manually clicked Reconnect (which then
// also failed to clean up in the round-2+ case — see
// TestCleanupThreadSynthesizesAfterRound2PlusReRound).
//
// After the fix, !s.closing is the only gate: any time the read loop
// exits without the host asking us to close, "error" fires.
func TestReadLoopEmitsErrorStatusOnCleanUnexpectedExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args:   []string{"-c", "sleep 0.05; exit 0"},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	eventCh := make(chan provider.ProviderEvent, 100)
	s := &Session{
		proc:     proc,
		threadID: testThread,
		onEvent: func(evt provider.ProviderEvent) {
			eventCh <- evt
		},
		cancel:   cancel,
		readDone: make(chan struct{}),
	}
	go s.readLoop()

	var gotError, gotDisconnected bool
	timeout := time.After(5 * time.Second)
	for !(gotError && gotDisconnected) {
		select {
		case evt := <-eventCh:
			if evt.Kind != provider.EventSessionStatus {
				continue
			}
			switch evt.Content {
			case "error":
				gotError = true
				// Clean-exit case: meta still round-trips (Reason field
				// carries the generic "exited unexpectedly" string when
				// the exit error itself is nil). The exact reason is not
				// pinned here — it can be either the zero-error generic
				// string or a real ExitError when Wait beats the 100ms
				// timeout — but the event MUST fire.
			case "disconnected":
				gotDisconnected = true
			}
		case <-timeout:
			t.Fatalf("timeout waiting for error+disconnected on clean unexpected exit (gotError=%v gotDisconnected=%v)", gotError, gotDisconnected)
		}
	}
}

func TestReadLoopEmitsErrorStatusOnUnexpectedExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args:   []string{"-c", "sleep 0.05; exit 7"},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	eventCh := make(chan provider.ProviderEvent, 100)
	s := &Session{
		proc:     proc,
		threadID: testThread,
		onEvent: func(evt provider.ProviderEvent) {
			eventCh <- evt
		},
		cancel:   cancel,
		readDone: make(chan struct{}),
	}
	go s.readLoop()

	var gotError, gotDisconnected bool
	timeout := time.After(5 * time.Second)
	for !(gotError && gotDisconnected) {
		select {
		case evt := <-eventCh:
			if evt.Kind != provider.EventSessionStatus {
				continue
			}
			switch evt.Content {
			case "error":
				gotError = true
				var meta provider.ProcessExitInfo
				if err := json.Unmarshal(evt.Meta, &meta); err != nil {
					t.Fatalf("unmarshal exit meta: %v", err)
				}
				if meta.ExitCode != 7 {
					t.Fatalf("exitCode = %d, want 7", meta.ExitCode)
				}
			case "disconnected":
				gotDisconnected = true
			}
		case <-timeout:
			t.Fatal("timeout waiting for unexpected-exit events")
		}
	}
}

// stopTaskResponderScript is a bash fake-CLI that reads stdin line by
// line and writes a canned control_response for every stop_task
// request it sees. The response shape is parameterised by mode:
//   - "success": subtype=success, echoes back the request_id
//   - "error":   subtype=error with a provider-side message
//   - "silent":  drops the line; never responds (timeout path)
//   - "stray":   writes a control_response with a different request_id
//     (unknown-id-dropped path)
//
// The script terminates when stdin closes (Session.Close → proc.Close
// shuts the pipe). Written to a temp file so the test doesn't rely on
// a specific shell quoting pattern.
func stopTaskResponderScript(mode string) string {
	const header = `#!/bin/sh
set -u
while IFS= read -r line; do
    case "$line" in
        *'"stop_task"'*)
            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
`
	const footer = `
            ;;
    esac
done
`
	var body string
	switch mode {
	case "success":
		body = `            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"`
	case "error":
		body = `            printf '{"type":"control_response","response":{"subtype":"error","request_id":"%s","error":"task not found"}}\n' "$reqid"`
	case "silent":
		body = `            : # drop the line deliberately to exercise the timeout path`
	case "stray":
		body = `            printf '{"type":"control_response","response":{"subtype":"success","request_id":"not-the-real-one","response":{}}}\n'`
	default:
		body = `            : # unknown mode — never happens in tests`
	}
	return header + body + footer
}

// newStopTaskResponderSession spawns a Session backed by the fake-CLI
// script returned by stopTaskResponderScript. Wraps the boilerplate
// shared by the four StopTask tests.
func newStopTaskResponderSession(t *testing.T, mode string, stopTimeout time.Duration) *Session {
	t.Helper()
	scriptDir := t.TempDir()
	scriptPath := scriptDir + "/fake-claude"
	if err := os.WriteFile(scriptPath, []byte(stopTaskResponderScript(mode)), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: scriptPath})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	s := &Session{
		proc:     proc,
		threadID: testThread,
		onEvent:  func(evt provider.ProviderEvent) { _ = evt },
		cancel:   cancel,
		readDone: make(chan struct{}),
		// Short timeout so a "silent" mode doesn't stall the suite.
		controlRequestTimeout: stopTimeout,
	}
	go s.readLoop()
	t.Cleanup(func() {
		_ = s.Close()
	})
	return s
}

// TestSession_StopTask_SuccessRoundTrip drives the happy path end to
// end: StopTask writes a stop_task control_request, the fake CLI
// matches the request_id and replies with subtype=success, and
// StopTask returns nil.
func TestSession_StopTask_SuccessRoundTrip(t *testing.T) {
	s := newStopTaskResponderSession(t, "success", 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.StopTask(ctx, "task-abc"); err != nil {
		t.Fatalf("StopTask success: %v", err)
	}
}

// TestSession_StopTask_ErrorResponse confirms that a subtype=error
// response surfaces as a non-nil error whose message contains the
// provider-supplied detail so the caller can render it to the user.
func TestSession_StopTask_ErrorResponse(t *testing.T) {
	s := newStopTaskResponderSession(t, "error", 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := s.StopTask(ctx, "task-bad")
	if err == nil {
		t.Fatal("StopTask error: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "task not found") {
		t.Errorf("error message missing server detail: %v", err)
	}
}

// TestSession_StopTask_Timeout exercises the watchdog: the fake CLI
// consumes the request and goes silent; StopTask must return a
// timeout error within the configured window.
func TestSession_StopTask_Timeout(t *testing.T) {
	// Use a generous test context so the timeout error comes from
	// Session.controlRequestTimeout, not the caller context.
	s := newStopTaskResponderSession(t, "silent", 150*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	err := s.StopTask(ctx, "task-wait")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("StopTask timeout: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("error should mention timeout, got: %v", err)
	}
	// Generous upper bound so flaky CI doesn't trip — we just want to
	// confirm the session didn't sit there for the 10s default.
	if elapsed > 2*time.Second {
		t.Errorf("StopTask took %s, expected near 150ms", elapsed)
	}
}

// TestSession_StopTask_EmptyTaskID fails fast when the caller passes
// a blank task_id — a silent no-op here would strand the per-row UI
// "Stop" button without feedback. TrimSpace covers stray-whitespace
// ids picked up from a UI surface.
//
// The test pins both the error MESSAGE (must mention empty task_id, NOT
// just "timeout") and the elapsed time — without the TrimSpace gate
// the call would write a stop_task to stdin, sit on the controlRequestTimeout
// for seconds, then return a timeout error that still trips `err != nil`
// but masks the programming bug. A tight 300ms ceiling proves we
// rejected the input without ever hitting the wire.
func TestSession_StopTask_EmptyTaskID(t *testing.T) {
	s := newStopTaskResponderSession(t, "silent", 5*time.Second)
	for _, tid := range []string{"", "   ", "\t\n"} {
		start := time.Now()
		err := s.StopTask(context.Background(), tid)
		elapsed := time.Since(start)
		if err == nil {
			t.Errorf("StopTask(%q): expected error, got nil", tid)
			continue
		}
		if !strings.Contains(err.Error(), "empty task_id") {
			t.Errorf("StopTask(%q): error should mention empty task_id, got: %v", tid, err)
		}
		// A missing TrimSpace would write to the fake CLI and wait the
		// full controlRequestTimeout (5s). 300ms is a generous upper bound that
		// still catches the regression.
		if elapsed > 300*time.Millisecond {
			t.Errorf("StopTask(%q) took %s, expected <300ms (suggests the input check fell through to the wire)", tid, elapsed)
		}
	}
}

// TestSession_StopTask_UnknownRequestIDDropped confirms the read loop
// silently drops control_response envelopes whose request_id doesn't
// match any pending StopTask. The in-flight StopTask still reaches
// its timeout and the session keeps processing lines.
func TestSession_StopTask_UnknownRequestIDDropped(t *testing.T) {
	s := newStopTaskResponderSession(t, "stray", 200*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := s.StopTask(ctx, "task-x")
	if err == nil {
		t.Fatal("StopTask with stray response: expected timeout, got nil")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("unexpected error shape: %v", err)
	}

	// Session must still be alive — a second call should work if we
	// wired one up. We don't have a second responder, so instead
	// confirm the subprocess hasn't died (the stray line didn't take
	// the read loop down).
	select {
	case <-s.proc.Done():
		t.Fatal("session died after stray control_response — read loop must survive unknown request_ids")
	default:
	}
}

// TestSession_StopTask_SubprocessDeathUnblocksCaller pins the behavior
// the readLoop's deferred clearPendingControlRequests buys us: when the CLI
// subprocess exits on its own while a StopTask is parked, the caller
// must unblock promptly with a clean error — NOT sit on its 10-second
// DefaultControlRequestTimeout waiting for a response that will never come.
// Without this guarantee the tray "Stop all" flow would freeze the UI
// for seconds per pending task after an unclean CLI exit.
func TestSession_StopTask_SubprocessDeathUnblocksCaller(t *testing.T) {
	// Fake CLI reads the first line (the stop_task), pauses briefly so
	// the StopTask goroutine is demonstrably parked on its pending
	// channel, then exits with status 0 — no response ever written.
	// This is exactly what an unclean subprocess death looks like to
	// the read loop: io.EOF with a pending caller.
	scriptDir := t.TempDir()
	scriptPath := scriptDir + "/fake-claude"
	script := `#!/bin/sh
set -u
# Drain exactly one line, then exit without writing anything back.
read -r _discard
sleep 0.05
exit 0
`
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: scriptPath})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	s := &Session{
		proc:     proc,
		threadID: testThread,
		onEvent:  func(evt provider.ProviderEvent) { _ = evt },
		cancel:   cancel,
		readDone: make(chan struct{}),
		// Generous per-call timeout so the fast-unblock comes from the
		// readLoop cleanup, not from the timeout path. A failing
		// regression would wait this entire window.
		controlRequestTimeout: 5 * time.Second,
	}
	go s.readLoop()
	t.Cleanup(func() { _ = s.Close() })

	start := time.Now()
	err = s.StopTask(context.Background(), "task-vanish")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("StopTask after subprocess death: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "session closed") {
		t.Errorf("error should mention session close, got: %v", err)
	}
	// The subprocess exits ~50ms in; the read loop then drains and
	// signals the pending entry via clearPendingControlRequests. 1s is a
	// generous upper bound that still proves we didn't silently wait
	// the full 5s timeout.
	if elapsed > time.Second {
		t.Errorf("StopTask took %s after subprocess death, expected <1s (suggests timeout path, not readLoop signal)", elapsed)
	}
}

// TestSession_StopTask_ConcurrentSameTaskIDDistinctRequestIDs pins the
// per-request correlation contract when the frontend's Stop-all fan-out
// (or a double-click on the per-row Stop button) issues two StopTask
// calls for the SAME task_id within the same session. Each outbound
// control_request must carry a unique request_id so the CLI's reply
// resolves back to the right pending channel — a regression that
// reused a single request_id across concurrent stops on the same
// task_id would race: the first success unblocks BOTH callers if the
// map keyed by task_id, or only one call would land if the second
// overwrote the pending entry.
//
// The allocateControlRequestID counter guarantees uniqueness per
// session; this test pins that behavior end-to-end by tripping the
// fake-CLI to echo back each request_id and checking both StopTask
// calls resolve with no cross-talk.
func TestSession_StopTask_ConcurrentSameTaskIDDistinctRequestIDs(t *testing.T) {
	s := newStopTaskResponderSession(t, "success", 3*time.Second)

	// Two goroutines: both call StopTask with the SAME task_id. Each
	// allocates its own request_id via the seq counter; the fake CLI
	// echoes back whichever request_id it read, and deliverControlResponse
	// routes the two replies to their respective pending channels.
	const task = "task-double"
	done := make(chan error, 2)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		done <- s.StopTask(ctx, task)
	}()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		done <- s.StopTask(ctx, task)
	}()

	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("concurrent StopTask #%d: %v", i, err)
			}
		case <-time.After(4 * time.Second):
			t.Fatalf("concurrent StopTask #%d never returned — request_id collision suspected", i)
		}
	}

	// The two request_ids must be distinct. allocateControlRequestID
	// bumps controlRequestSeq under a lock, so two serialized StopTask calls
	// (the fake CLI's `read -r` is line-buffered) land at seq=1 and
	// seq=2. Observing seq >= 2 is sufficient — the third slot is
	// unallocated.
	s.controlRequestMu.Lock()
	seq := s.controlRequestSeq
	s.controlRequestMu.Unlock()
	if seq < 2 {
		t.Errorf("controlRequestSeq = %d after two concurrent StopTasks, want >= 2 (each call must allocate)", seq)
	}
}

// TestWithClaudeSessionEnvDefaults pins the env-merge helper that tags
// every spawned `claude` subprocess with
// `CLAUDE_CODE_ENTRYPOINT=agent-overflow` and opts it into the todo
// tool surface with `CLAUDE_CODE_ENABLE_TODO_TOOLS=true`.
//
// The CLI's resume picker filters sessions whose entrypoint is `sdk-cli`
// (the auto-detected default for stream-json invocations); setting our
// own value keeps agent-overflow's threads resumable from a normal
// `claude --resume`. See docs/references/claude.md and the `Ka8(H)`
// override in the binary that rewrites the literal string `"cli"` to
// `"sdk-cli"` — any other preset value survives. The todo opt-in exists
// because claude ≥2.1.233 removes TodoWrite/Task* for modern models
// unless the session opts back in (claudeTodoToolsEnvVar's comment has
// the gate details).
func TestWithClaudeSessionEnvDefaults(t *testing.T) {
	t.Run("nil env gets both defaults set", func(t *testing.T) {
		got := withClaudeSessionEnvDefaults(nil)
		if got["CLAUDE_CODE_ENTRYPOINT"] != "agent-overflow" {
			t.Fatalf("CLAUDE_CODE_ENTRYPOINT = %q, want agent-overflow", got["CLAUDE_CODE_ENTRYPOINT"])
		}
		if got["CLAUDE_CODE_ENABLE_TODO_TOOLS"] != "true" {
			t.Fatalf("CLAUDE_CODE_ENABLE_TODO_TOOLS = %q, want true", got["CLAUDE_CODE_ENABLE_TODO_TOOLS"])
		}
	})

	t.Run("preserves caller-provided keys", func(t *testing.T) {
		got := withClaudeSessionEnvDefaults(map[string]string{"FOO": "bar"})
		if got["FOO"] != "bar" {
			t.Errorf("FOO clobbered: got %q, want bar", got["FOO"])
		}
		if got["CLAUDE_CODE_ENTRYPOINT"] != "agent-overflow" {
			t.Errorf("CLAUDE_CODE_ENTRYPOINT not added: got %q", got["CLAUDE_CODE_ENTRYPOINT"])
		}
		if got["CLAUDE_CODE_ENABLE_TODO_TOOLS"] != "true" {
			t.Errorf("CLAUDE_CODE_ENABLE_TODO_TOOLS not added: got %q", got["CLAUDE_CODE_ENABLE_TODO_TOOLS"])
		}
	})

	t.Run("respects caller-provided overrides per variable", func(t *testing.T) {
		got := withClaudeSessionEnvDefaults(map[string]string{
			"CLAUDE_CODE_ENTRYPOINT": "test-override",
		})
		if got["CLAUDE_CODE_ENTRYPOINT"] != "test-override" {
			t.Errorf("entrypoint override clobbered: got %q, want test-override", got["CLAUDE_CODE_ENTRYPOINT"])
		}
		// The other default still applies — opting out of one variable
		// must not opt out of the rest.
		if got["CLAUDE_CODE_ENABLE_TODO_TOOLS"] != "true" {
			t.Errorf("CLAUDE_CODE_ENABLE_TODO_TOOLS not added alongside an entrypoint override: got %q", got["CLAUDE_CODE_ENABLE_TODO_TOOLS"])
		}
	})

	t.Run("user can disable the todo opt-in", func(t *testing.T) {
		got := withClaudeSessionEnvDefaults(map[string]string{
			"CLAUDE_CODE_ENABLE_TODO_TOOLS": "false",
		})
		if got["CLAUDE_CODE_ENABLE_TODO_TOOLS"] != "false" {
			t.Errorf("todo opt-out clobbered: got %q, want false", got["CLAUDE_CODE_ENABLE_TODO_TOOLS"])
		}
	})

	t.Run("returns the same map when every default is present", func(t *testing.T) {
		input := map[string]string{
			"CLAUDE_CODE_ENTRYPOINT":        "x",
			"CLAUDE_CODE_ENABLE_TODO_TOOLS": "false",
		}
		got := withClaudeSessionEnvDefaults(input)
		// Maps are reference types: a write through the return proves it
		// is the caller's map, not a pointless copy.
		got["PROBE"] = "1"
		if input["PROBE"] != "1" {
			t.Errorf("expected the input map back unchanged when nothing is missing")
		}
	})

	t.Run("does not mutate caller's map", func(t *testing.T) {
		input := map[string]string{"FOO": "bar"}
		_ = withClaudeSessionEnvDefaults(input)
		if len(input) != 1 {
			t.Errorf("input map was mutated; helper must return a copy")
		}
	})
}

// TestAutoModeSurfacesFallbackApprovalRequest is the safety net under the auto
// runtime mode. Claude's auto classifier does NOT answer every request: it
// falls back to a real interactive ask on safety_check, ask_rule,
// plan_mode_floor, org_ask_ceiling and requires_user_interaction, and the
// fallback arrives as an ordinary `can_use_tool` control_request. If AO ever
// swallowed or auto-answered those, an auto-mode turn would hang on a prompt
// the user never sees (or, worse, silently allow what the classifier declined
// to bless).
//
// The one path that could swallow it is handleFullAccessToolRequest, whose
// auto-approval short-circuit is keyed on the literal permission mode
// "bypassPermissions". Auto is a different mode with a different promise — the
// reviewer can DENY — so this asserts the full round trip on an auto session:
// the request surfaces as EventApprovalRequest and RespondToApproval resolves
// it exactly as it would in any other tier.
func TestAutoModeSurfacesFallbackApprovalRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: "cat"})
	if err != nil {
		t.Fatalf("spawn cat: %v", err)
	}
	eventCh := make(chan provider.ProviderEvent, 200)
	s := &Session{
		proc:                  proc,
		threadID:              testThread,
		onEvent:               func(evt provider.ProviderEvent) { eventCh <- evt },
		cancel:                cancel,
		readDone:              make(chan struct{}),
		basePermissionMode:    claudeBasePermissionMode(provider.RuntimeAuto),
		currentPermissionMode: claudeBasePermissionMode(provider.RuntimeAuto),
	}
	go s.readLoop()
	t.Cleanup(func() {
		cancel()
		proc.Close()
	})

	// Guard the premise: the mode under test really is auto, not a typo that
	// happens to miss the bypassPermissions branch for the wrong reason.
	if got := s.getCurrentPermissionMode(); got != "auto" {
		t.Fatalf("currentPermissionMode = %q, want auto", got)
	}

	line := []byte(`{"type":"control_request","request_id":"req-auto-fallback","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"rm -rf /tmp/x"}}}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write approval: %v", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case evt := <-eventCh:
			if evt.Kind != provider.EventApprovalRequest {
				continue
			}
			var approval provider.ApprovalRequest
			if err := json.Unmarshal(evt.Meta, &approval); err != nil {
				t.Fatalf("unmarshal approval: %v", err)
			}
			if approval.RequestID != "req-auto-fallback" || approval.ToolName != "Bash" {
				t.Fatalf("approval = %+v, want the Bash request that was sent", approval)
			}
			if err := s.RespondToApproval(context.Background(), provider.ApprovalResponse{
				RequestID: "req-auto-fallback",
				Decision:  "allow",
			}); err != nil {
				t.Fatalf("RespondToApproval: %v", err)
			}
			return
		case <-deadline:
			t.Fatal("auto-mode session never surfaced the fallback approval request")
		}
	}
}

// TestAutoModeDoesNotAutoApproveToolRequests is the unit-level twin of the
// round trip above: the full-access short-circuit must decline to claim an
// auto-mode request. Stated separately because the two failures are different
// bugs — this one would auto-ALLOW a tool the classifier had already refused
// to bless, which no event assertion downstream could detect.
func TestAutoModeDoesNotAutoApproveToolRequests(t *testing.T) {
	s := &Session{currentPermissionMode: claudeBasePermissionMode(provider.RuntimeAuto)}
	handled, err := s.maybeHandleFullAccessToolRequest(
		[]byte(`{"type":"control_request","request_id":"req-1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`))
	if err != nil {
		t.Fatalf("maybeHandleFullAccessToolRequest: %v", err)
	}
	if handled {
		t.Fatal("auto-mode session auto-approved a tool request; only bypassPermissions may do that")
	}
}
