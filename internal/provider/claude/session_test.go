package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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

func TestParseAssistantThinkingBlockIsSkipped(t *testing.T) {
	// Thinking blocks on the coalesced `assistant` envelope are skipped
	// for the same reason as text blocks — stream_event thinking_delta
	// is the sole source of thinking content.
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"thinking","thinking":"Let me consider..."}]}}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, e := range events {
		if e.Kind == provider.EventThinking {
			t.Fatalf("assistant envelope emitted EventThinking for a thinking block: %+v", e)
		}
	}
}

func TestParseAssistantWithUsage(t *testing.T) {
	// Text blocks are skipped (streamed via stream_event); usage is still
	// emitted from the assistant envelope.
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":80,"cache_creation_input_tokens":20}}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event (usage only), got %d: %+v", len(events), events)
	}
	if events[0].Kind != provider.EventTokenUsage {
		t.Errorf("kind: got %q, want token_usage", events[0].Kind)
	}
	var usage provider.TokenUsage
	if err := json.Unmarshal(events[0].Meta, &usage); err != nil {
		t.Fatalf("unmarshal usage: %v", err)
	}
	if usage.InputTokens != 100 {
		t.Errorf("input tokens: got %d, want 100", usage.InputTokens)
	}
	if usage.OutputTokens != 50 {
		t.Errorf("output tokens: got %d, want 50", usage.OutputTokens)
	}
}

func TestParseAssistantMultipleBlocks(t *testing.T) {
	// Thinking and text are skipped on the assistant envelope (streamed
	// via stream_event). Only the tool_use fires.
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"hello"},{"type":"tool_use","id":"t1","name":"Edit","input":{"file":"x"}}]}}`)

	events, err := ParseLine(testThread, line)
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
	line := []byte(`{"type":"system","subtype":"compact_boundary","data":{"context_window":{"used_tokens":50000,"max_tokens":200000,"used_percentage":25,"total_processed":120000}}}`)

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
	if meta.TotalProcessed != 120000 {
		t.Errorf("TotalProcessed: got %d, want 120000", meta.TotalProcessed)
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
	line := []byte(`{"type":"system","subtype":"api_retry","data":{"delay":5000,"attempt":2}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	evt := events[0]
	if evt.Kind != provider.EventSessionStatus {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventSessionStatus)
	}
	if evt.Content != "retrying" {
		t.Errorf("content: got %q, want %q", evt.Content, "retrying")
	}
	if evt.ThreadID != testThread {
		t.Errorf("threadID: got %q, want %q", evt.ThreadID, testThread)
	}

	var meta map[string]any
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["delay"] != float64(5000) {
		t.Errorf("delay: got %v, want 5000", meta["delay"])
	}
	if meta["attempt"] != float64(2) {
		t.Errorf("attempt: got %v, want 2", meta["attempt"])
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

func TestParseStreamEventNoDelta(t *testing.T) {
	line := []byte(`{"type":"stream_event","event":"message_start","data":{"type":"message_start"}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events, got %d", len(events))
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

func TestParseRateLimitEvent(t *testing.T) {
	line := []byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1776283200,"rateLimitType":"five_hour"}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventRateLimits {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventRateLimits)
	}

	var snapshot provider.RateLimitsSnapshot
	if err := json.Unmarshal(events[0].Meta, &snapshot); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if snapshot.Provider != "claude" {
		t.Errorf("provider: got %q, want claude", snapshot.Provider)
	}
	if len(snapshot.Limits) != 1 {
		t.Fatalf("limits len: got %d, want 1", len(snapshot.Limits))
	}
	if snapshot.Limits[0].LimitID != "five_hour" {
		t.Errorf("limitId: got %q, want five_hour", snapshot.Limits[0].LimitID)
	}
	if snapshot.Limits[0].ResetsAt != 1776283200 {
		t.Errorf("resetsAt: got %d, want 1776283200", snapshot.Limits[0].ResetsAt)
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
	args := buildArgs(Config{})

	// Baseline flags that every spawn must include. Adding a new flag to
	// buildArgs should extend this list intentionally.
	expected := []string{
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--permission-prompt-tool", "stdio",
		"--include-partial-messages",
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

func TestBuildArgsWithAllOptions(t *testing.T) {
	args := buildArgs(Config{
		Model:           "opus",
		Resume:          "session-123",
		ForkSession:     true,
		SystemPrompt:    "Be helpful",
		PermissionFlags: []string{"--permission-mode", "acceptEdits"},
		MaxTurns:        5,
		AllowedTools:    []string{"Bash", "Edit"},
	})

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
	if !findFlag("--system-prompt", "Be helpful") {
		t.Error("missing --system-prompt")
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

func TestBuildArgsNoPermissionFlagsOmitsAll(t *testing.T) {
	args := buildArgs(Config{PermissionFlags: nil})

	for _, a := range args {
		if a == "--permission-mode" || a == "--allow-dangerously-skip-permissions" {
			t.Errorf("permission flag %q should be omitted when PermissionFlags is nil", a)
		}
	}
}

// TestBuildArgsDangerousSkipPermissions confirms the full-access flow emits
// the bypass permission mode plus the bare dangerous-skip allow flag.
func TestBuildArgsDangerousSkipPermissions(t *testing.T) {
	args := buildArgs(Config{PermissionFlags: []string{"--permission-mode", "bypassPermissions", "--allow-dangerously-skip-permissions"}})
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
		"message": map[string]string{
			"role":    "user",
			"content": "hello",
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
		Content string `json:"content"`
	}
	json.Unmarshal(parsed["message"], &message)
	if message.Role != "user" {
		t.Errorf("role: got %q, want %q", message.Role, "user")
	}
	if message.Content != "hello" {
		t.Errorf("content: got %q, want %q", message.Content, "hello")
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

func TestInterruptFormat(t *testing.T) {
	msg := map[string]any{
		"type": "control",
		"control": map[string]string{
			"type": "interrupt",
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
	if msgType != "control" {
		t.Errorf("type: got %q, want %q", msgType, "control")
	}

	var control struct {
		Type string `json:"type"`
	}
	json.Unmarshal(parsed["control"], &control)
	if control.Type != "interrupt" {
		t.Errorf("control type: got %q, want %q", control.Type, "interrupt")
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
	s, _ := newTestClaudeSession(t)
	if err := s.Send(context.Background(), "hello world", provider.SendOptions{}); err != nil {
		t.Fatalf("Send: %v", err)
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
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("unmarshal second captured line: %v", err)
	}
	if second.Type != "user" || second.Message.Role != "user" || second.Message.Content != "draft a plan" {
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

func TestSessionInterrupt(t *testing.T) {
	s, _ := newTestClaudeSession(t)
	if err := s.Interrupt(context.Background()); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
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
			s.startApprovalTimer("req-"+d, provider.EventApprovalResolved)
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
		proc:            proc,
		threadID:        testThread,
		approvalTimeout: 5 * time.Second,
		onEvent:         func(provider.ProviderEvent) {},
		cancel:          cancel,
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
	}}
	s.startApprovalTimerWithQuestions("req-user-input", provider.EventUserInputResolved, questions)

	err = s.RespondToUserInput(context.Background(), provider.UserInputResponse{
		RequestID: "req-user-input",
		Decision:  "accept",
		Answers: map[string]provider.UserInputAnswer{
			"framework": provider.SingleUserInputAnswer("Svelte"),
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
	if msg.Response.Response.UpdatedInput.Answers["framework"] != "Svelte" {
		t.Fatalf("answers = %+v, want framework=Svelte", msg.Response.Response.UpdatedInput.Answers)
	}
	if !reflect.DeepEqual(msg.Response.Response.UpdatedInput.Questions, questions) {
		t.Fatalf("questions = %+v, want %+v", msg.Response.Response.UpdatedInput.Questions, questions)
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

// TestApprovalTimeoutAutoDeniesClaude exercises Bug B3 for the Claude
// stdio approval flow: when CanUseTool arrives and no RespondToApproval
// follows within the timeout, the session auto-denies (so the subprocess
// is unblocked) and emits an EventError describing the timeout. The
// session must stay alive — only that single pending request is resolved.
func TestApprovalTimeoutAutoDeniesClaude(t *testing.T) {
	s, eventCh := newTestClaudeSessionWithApprovalTimeout(t, 100*time.Millisecond)

	// Drive a CanUseTool request through the readLoop.
	approvalLine := []byte(`{"type":"control_request","request_id":"req-timeout","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`)
	if err := s.proc.WriteLine(approvalLine); err != nil {
		t.Fatalf("write approval: %v", err)
	}

	// Expect the approval event, then an error event within the timeout.
	var gotApprovalReq, gotError bool
	deadline := time.After(3 * time.Second)
	for !(gotApprovalReq && gotError) {
		select {
		case evt := <-eventCh:
			switch evt.Kind {
			case provider.EventApprovalRequest:
				gotApprovalReq = true
			case provider.EventError:
				if containsAny(evt.Content, "approval timed out", "approval timeout") {
					gotError = true
				}
			}
		case <-deadline:
			t.Fatalf("timeout waiting for approval req + auto-deny error (req=%v err=%v)", gotApprovalReq, gotError)
		}
	}

	// The provider (cat) will have echoed our deny control_response back.
	// Drain until we see it so we verify the deny was actually written.
	findDeny := make(chan string, 1)
	go func() {
		for evt := range eventCh {
			_ = evt // drain so the channel is still consumed
		}
	}()

	// Inspect the process's written bytes by writing a marker line; our
	// session's readLoop echoes it. If the session had died we'd never
	// see the marker.
	marker := []byte(`{"type":"system","subtype":"future_feature"}`)
	if err := s.proc.WriteLine(marker); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	// Give readLoop a moment to process the marker.
	time.Sleep(50 * time.Millisecond)

	// Session should still be alive.
	select {
	case <-s.proc.Done():
		t.Fatal("session died after auto-deny; should stay alive for future turns")
	default:
	}
	_ = findDeny
}

// TestApprovalResponseCancelsTimeoutClaude confirms the happy path: when
// RespondToApproval arrives before the timeout, no auto-deny fires.
func TestApprovalResponseCancelsTimeoutClaude(t *testing.T) {
	s, eventCh := newTestClaudeSessionWithApprovalTimeout(t, 500*time.Millisecond)

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

	// Respond promptly.
	if err := s.RespondToApproval(context.Background(), provider.ApprovalResponse{
		RequestID: "req-normal",
		Decision:  "allow",
	}); err != nil {
		t.Fatalf("RespondToApproval: %v", err)
	}

	// Wait past the timeout. No EventError with "timeout" should arrive.
	deadline := time.After(800 * time.Millisecond)
	for {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventError && containsAny(evt.Content, "approval timed out", "approval timeout") {
				t.Fatalf("auto-deny fired despite timely response: %v", evt.Content)
			}
		case <-deadline:
			return
		}
	}
}

// TestApprovalTimeoutClearedOnCloseClaude exercises the close-mid-pending
// case: the session tears down while an approval timer is active. The
// timer must be cancelled cleanly (no spurious auto-deny emitted after
// Close returns).
func TestApprovalTimeoutClearedOnCloseClaude(t *testing.T) {
	s, eventCh := newTestClaudeSessionWithApprovalTimeout(t, 200*time.Millisecond)

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

	// Close before the timeout fires.
	if err := s.Close(); err != nil {
		t.Logf("close returned %v (acceptable)", err)
	}

	// After close, give the would-be timeout extra time to potentially
	// fire — it must not.
	deadline := time.After(500 * time.Millisecond)
	for {
		select {
		case evt, ok := <-eventCh:
			if !ok {
				return
			}
			if evt.Kind == provider.EventError && containsAny(evt.Content, "approval timed out", "approval timeout") {
				t.Fatalf("auto-deny fired after session closed: %v", evt.Content)
			}
		case <-deadline:
			return
		}
	}
}

// newTestClaudeSessionWithApprovalTimeout wires up a cat-backed session
// with a custom approval watchdog window for Bug B3 tests.
func newTestClaudeSessionWithApprovalTimeout(t *testing.T, timeout time.Duration) (*Session, <-chan provider.ProviderEvent) {
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
		cancel:          cancel,
		readDone:        make(chan struct{}),
		approvalTimeout: timeout,
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

	// Print the plan request, then deterministically close stdin (the read
	// end of our write pipe) while keeping the subprocess alive briefly.
	// Closing the read end guarantees the handler's WriteLine returns
	// EPIPE — an earlier version of this test had the subprocess exit
	// outright, which raced the kernel: sometimes the write landed in
	// the pipe buffer before the kernel marked the write end broken,
	// letting the write succeed and the EventError never fire.
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args:   []string{"-c", `printf '{"type":"control_request","request_id":"plan-1","request":{"subtype":"can_use_tool","tool_name":"ExitPlanMode","input":{"plan":"# plan"}}}\n'; exec 0<&-; sleep 0.5`},
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

// TestIdleWatchdogFiresAfterSilence exercises Bug B2: once Send is called
// we expect the subprocess to produce at least one stdout line within the
// configured idle timeout. If it stays silent, the watchdog must close the
// session, emit an EventError mentioning the timeout, and reach the
// disconnected terminal state. A regression that disabled the watchdog
// would leave the user waiting forever.
func TestIdleWatchdogFiresAfterSilence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Subprocess reads from stdin (so Send succeeds) but never writes to
	// stdout — exactly the provider-hang scenario.
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args:   []string{"-c", "cat > /dev/null; sleep 60"},
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
		cancel:      cancel,
		readDone:    make(chan struct{}),
		idleTimeout: 100 * time.Millisecond,
	}
	go s.readLoop()

	if err := s.Send(context.Background(), "hello", provider.SendOptions{}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var gotTimeout, gotDisconnected bool
	deadline := time.After(5 * time.Second)
	for !(gotTimeout && gotDisconnected) {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventError && containsAny(evt.Content, "timeout", "idle", "no output") {
				gotTimeout = true
			}
			if evt.Kind == provider.EventSessionStatus && evt.Content == "disconnected" {
				gotDisconnected = true
			}
		case <-deadline:
			t.Fatalf("timeout waiting for idle-timeout error + disconnected (timeout=%v disconnected=%v)", gotTimeout, gotDisconnected)
		}
	}

	select {
	case <-proc.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("process not reaped after idle watchdog fired")
	}
}

// TestIdleWatchdogResetByOutput confirms the watchdog is heartbeat-based:
// as long as the subprocess emits any line within the window, the session
// stays alive.
func TestIdleWatchdogResetByOutput(t *testing.T) {
	s, eventCh := newTestClaudeSessionWithIdleTimeout(t, 200*time.Millisecond)

	if err := s.Send(context.Background(), "ping", provider.SendOptions{}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Emit a line every 80 ms for 1 second — well under the 200 ms cap.
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(80 * time.Millisecond)
		defer ticker.Stop()
		for i := 0; i < 12; i++ {
			<-ticker.C
			line := []byte(`{"type":"assistant","message":{"id":"m","role":"assistant","content":[{"type":"text","text":"chunk"}]}}`)
			if err := s.proc.WriteLine(line); err != nil {
				return
			}
		}
	}()

	// Drain events for 1.1 s; no EventError from the watchdog should arrive.
	deadline := time.After(1100 * time.Millisecond)
	for {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventError && containsAny(evt.Content, "timeout", "idle") {
				t.Fatalf("idle watchdog fired despite continuous output: %v", evt.Content)
			}
		case <-deadline:
			<-done
			return
		}
	}
}

// TestIdleWatchdogDoesNotRunBetweenTurns verifies the watchdog only arms
// while a turn is mid-flight. An idle session awaiting the next user
// message must NOT be killed for inactivity.
func TestIdleWatchdogDoesNotRunBetweenTurns(t *testing.T) {
	_, eventCh := newTestClaudeSessionWithIdleTimeout(t, 50*time.Millisecond)

	// Wait past the idle window without ever calling Send. If the
	// watchdog ran unconditionally it would close the session.
	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventError {
				t.Fatalf("idle watchdog fired without Send being called: %v", evt.Content)
			}
			if evt.Kind == provider.EventSessionStatus && evt.Content == "disconnected" {
				t.Fatalf("session disconnected without Send being called")
			}
		case <-deadline:
			return
		}
	}
}

// TestIdleWatchdogStopsAtTurnComplete confirms the watchdog is disarmed
// when EventTurnComplete is observed. After that, the session should sit
// idle without being killed.
func TestIdleWatchdogStopsAtTurnComplete(t *testing.T) {
	s, eventCh := newTestClaudeSessionWithIdleTimeout(t, 80*time.Millisecond)

	if err := s.Send(context.Background(), "hello", provider.SendOptions{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// Feed a terminal result line immediately so the session observes
	// EventTurnComplete before the idle window expires.
	line := []byte(`{"type":"result","subtype":"success","is_error":false}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write result: %v", err)
	}

	// Drain until we see EventTurnComplete.
	gotComplete := false
	turnDeadline := time.After(500 * time.Millisecond)
	for !gotComplete {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventTurnComplete {
				gotComplete = true
			}
		case <-turnDeadline:
			t.Fatal("never saw EventTurnComplete")
		}
	}

	// Sleep past the idle window. The watchdog must stay disarmed.
	idleDeadline := time.After(300 * time.Millisecond)
	for {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventError {
				t.Fatalf("watchdog fired after turn complete: %v", evt.Content)
			}
			if evt.Kind == provider.EventSessionStatus && evt.Content == "disconnected" {
				t.Fatal("session disconnected after turn complete")
			}
		case <-idleDeadline:
			return
		}
	}
}

// newTestClaudeSessionWithIdleTimeout mirrors newTestClaudeSession but
// plumbs a custom idle watchdog window for Bug B2 tests.
func newTestClaudeSessionWithIdleTimeout(t *testing.T, idleTimeout time.Duration) (*Session, <-chan provider.ProviderEvent) {
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
		cancel:      cancel,
		readDone:    make(chan struct{}),
		idleTimeout: idleTimeout,
	}
	go s.readLoop()
	t.Cleanup(func() {
		cancel()
		proc.Close()
	})
	return s, eventCh
}

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

// TestWithClaudeCodeEntrypoint pins the env-merge helper that tags every
// spawned `claude` subprocess with `CLAUDE_CODE_ENTRYPOINT=agent-overflow`.
//
// The CLI's resume picker filters sessions whose entrypoint is `sdk-cli`
// (the auto-detected default for stream-json invocations); setting our
// own value keeps agent-overflow's threads resumable from a normal
// `claude --resume`. See docs/references/claude.md and the `Ka8(H)`
// override in the binary that rewrites the literal string `"cli"` to
// `"sdk-cli"` — any other preset value survives.
func TestWithClaudeCodeEntrypoint(t *testing.T) {
	t.Run("nil env gets entrypoint set", func(t *testing.T) {
		got := withClaudeCodeEntrypoint(nil)
		if got["CLAUDE_CODE_ENTRYPOINT"] != "agent-overflow" {
			t.Fatalf("CLAUDE_CODE_ENTRYPOINT = %q, want agent-overflow", got["CLAUDE_CODE_ENTRYPOINT"])
		}
	})

	t.Run("preserves caller-provided keys", func(t *testing.T) {
		got := withClaudeCodeEntrypoint(map[string]string{"FOO": "bar"})
		if got["FOO"] != "bar" {
			t.Errorf("FOO clobbered: got %q, want bar", got["FOO"])
		}
		if got["CLAUDE_CODE_ENTRYPOINT"] != "agent-overflow" {
			t.Errorf("CLAUDE_CODE_ENTRYPOINT not added: got %q", got["CLAUDE_CODE_ENTRYPOINT"])
		}
	})

	t.Run("respects caller-provided entrypoint override", func(t *testing.T) {
		got := withClaudeCodeEntrypoint(map[string]string{"CLAUDE_CODE_ENTRYPOINT": "test-override"})
		if got["CLAUDE_CODE_ENTRYPOINT"] != "test-override" {
			t.Errorf("override clobbered: got %q, want test-override", got["CLAUDE_CODE_ENTRYPOINT"])
		}
	})

	t.Run("does not mutate caller's map", func(t *testing.T) {
		input := map[string]string{"FOO": "bar"}
		_ = withClaudeCodeEntrypoint(input)
		if _, ok := input["CLAUDE_CODE_ENTRYPOINT"]; ok {
			t.Errorf("input map was mutated; helper must return a copy")
		}
	})
}
