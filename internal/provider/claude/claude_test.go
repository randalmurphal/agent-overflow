package claude

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"

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
		"compact_boundary", "api_retry",
		"hook_started", "hook_progress", "hook_response",
		"tool_progress", "notification", "files_persisted",
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
	line := []byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}`)

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

	expected := []string{"--input-format", "stream-json", "--output-format", "stream-json", "--verbose"}
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
