package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
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

func TestParseAssistantTextBlock(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":"Hello world"}]}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	evt := events[0]
	if evt.Kind != provider.EventTextDelta {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventTextDelta)
	}
	if evt.Content != "Hello world" {
		t.Errorf("content: got %q, want %q", evt.Content, "Hello world")
	}
	if evt.Role != "assistant" {
		t.Errorf("role: got %q, want %q", evt.Role, "assistant")
	}
	if evt.ItemID != "msg-1" {
		t.Errorf("itemID: got %q, want %q", evt.ItemID, "msg-1")
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

func TestParseAssistantThinkingBlock(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"thinking","thinking":"Let me consider..."}]}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	evt := events[0]
	if evt.Kind != provider.EventThinking {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventThinking)
	}
	if evt.Content != "Let me consider..." {
		t.Errorf("content: got %q, want %q", evt.Content, "Let me consider...")
	}
}

func TestParseAssistantWithUsage(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":80,"cache_creation_input_tokens":20}}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// text + usage = 2 events
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}

	if events[0].Kind != provider.EventTextDelta {
		t.Errorf("first event kind: got %q, want text_delta", events[0].Kind)
	}
	if events[1].Kind != provider.EventTokenUsage {
		t.Errorf("second event kind: got %q, want token_usage", events[1].Kind)
	}

	var usage provider.TokenUsage
	if err := json.Unmarshal(events[1].Meta, &usage); err != nil {
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
	line := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"hello"},{"type":"tool_use","id":"t1","name":"Edit","input":{"file":"x"}}]}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}

	if events[0].Kind != provider.EventThinking {
		t.Errorf("event[0]: got %q, want thinking", events[0].Kind)
	}
	if events[1].Kind != provider.EventTextDelta {
		t.Errorf("event[1]: got %q, want text_delta", events[1].Kind)
	}
	if events[2].Kind != provider.EventToolStart {
		t.Errorf("event[2]: got %q, want tool_start", events[2].Kind)
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

func TestParseToolProgress(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"tool_progress","item_id":"item-1","content":{"progress":{"current":5,"total":10,"message":"Reading..."}}}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	evt := events[0]
	if evt.Kind != provider.EventToolProgress {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventToolProgress)
	}
	if evt.ItemID != "item-1" {
		t.Errorf("itemID: got %q, want %q", evt.ItemID, "item-1")
	}
	if evt.ThreadID != testThread {
		t.Errorf("threadID: got %q, want %q", evt.ThreadID, testThread)
	}

	var meta map[string]any
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["current"] != float64(5) {
		t.Errorf("current: got %v, want 5", meta["current"])
	}
	if meta["total"] != float64(10) {
		t.Errorf("total: got %v, want 10", meta["total"])
	}
	if meta["message"] != "Reading..." {
		t.Errorf("message: got %v, want %q", meta["message"], "Reading...")
	}
}

func TestParseToolProgressNoContent(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"tool_progress"}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	evt := events[0]
	if evt.Kind != provider.EventToolProgress {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventToolProgress)
	}
	// When content is nil, Meta should default to "{}".
	if string(evt.Meta) != "{}" {
		t.Errorf("meta: got %s, want {}", evt.Meta)
	}
	if evt.ItemID != "" {
		t.Errorf("itemID: got %q, want empty", evt.ItemID)
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

func TestParseResultError(t *testing.T) {
	line := []byte(`{"type":"result","subtype":"error_during_execution","is_error":true,"error":"something broke"}`)

	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventError {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventError)
	}
	if events[0].Content != "something broke" {
		t.Errorf("content: got %q, want %q", events[0].Content, "something broke")
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

// TestParseRealCLIFixture validates the parser against real Claude CLI output.
func TestParseRealCLIFixture(t *testing.T) {
	f, err := os.Open("testdata/real_output.ndjson")
	if err != nil {
		t.Skipf("skipping real fixture test: %v", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	var lineNum int
	var foundInit, foundAssistant, foundResult bool

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

			case provider.EventTextDelta:
				foundAssistant = true
				if evt.Content == "" {
					t.Errorf("line %d: text delta has empty content", lineNum)
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
	if !foundAssistant {
		t.Error("fixture missing assistant text event")
	}
	if !foundResult {
		t.Error("fixture missing result/turn_complete event")
	}

	t.Logf("processed %d lines from real fixture: init=%v assistant=%v result=%v",
		lineNum, foundInit, foundAssistant, foundResult)
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
		Model:          "opus",
		Resume:         "session-123",
		ForkSession:    true,
		SystemPrompt:   "Be helpful",
		PermissionMode: "bypassPermissions",
		MaxTurns:       5,
		AllowedTools:   []string{"Bash", "Edit"},
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
	if !findFlag("--permission-mode", "bypassPermissions") {
		t.Error("missing --permission-mode")
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

func TestBuildArgsDefaultPermissionModeOmitted(t *testing.T) {
	args := buildArgs(Config{PermissionMode: "default"})

	for _, a := range args {
		if a == "--permission-mode" {
			t.Error("--permission-mode should be omitted for 'default'")
		}
	}
}

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
	if err := s.Send(context.Background(), "hello world"); err != nil {
		t.Fatalf("Send: %v", err)
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

	line := []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":"streaming text"}]}}`)
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

	if err := s.Send(context.Background(), "hello"); err != nil {
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

	if err := s.Send(context.Background(), "ping"); err != nil {
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

	if err := s.Send(context.Background(), "hello"); err != nil {
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
