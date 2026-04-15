package codex

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

const testThread = "thread-test"

// -- JSON helper tests --

func TestReadNestedString(t *testing.T) {
	data := json.RawMessage(`{"turn":{"id":"t1","status":"completed"}}`)

	if got := readNestedString(data, "turn", "id"); got != "t1" {
		t.Errorf("got %q, want %q", got, "t1")
	}
	if got := readNestedString(data, "turn", "status"); got != "completed" {
		t.Errorf("got %q, want %q", got, "completed")
	}
	if got := readNestedString(data, "turn", "missing"); got != "" {
		t.Errorf("got %q, want empty string", got)
	}
	if got := readNestedString(data, "missing", "id"); got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

func TestReadNestedStringDeep(t *testing.T) {
	data := json.RawMessage(`{"turn":{"error":{"message":"something broke"}}}`)
	if got := readNestedString(data, "turn", "error", "message"); got != "something broke" {
		t.Errorf("got %q, want %q", got, "something broke")
	}
}

func TestReadTopLevelString(t *testing.T) {
	data := json.RawMessage(`{"delta":"hello","other":42}`)

	if got := readTopLevelString(data, "delta"); got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
	if got := readTopLevelString(data, "missing"); got != "" {
		t.Errorf("got %q, want empty string", got)
	}
	// Non-string value should return empty.
	if got := readTopLevelString(data, "other"); got != "" {
		t.Errorf("got %q, want empty string for non-string value", got)
	}
}

func TestReadTopLevelStringInvalidJSON(t *testing.T) {
	if got := readTopLevelString(json.RawMessage(`not json`), "key"); got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

func TestReadNestedStringInvalidJSON(t *testing.T) {
	if got := readNestedString(json.RawMessage(`not json`), "key"); got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

// -- ClassifyNotification tests --

func TestTurnStarted(t *testing.T) {
	params := json.RawMessage(`{"turn":{"id":"turn-1"}}`)
	events := ClassifyNotification(testThread, "turn/started", params)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventTurnStart {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventTurnStart)
	}
	if events[0].TurnID != "turn-1" {
		t.Errorf("turnID: got %q, want %q", events[0].TurnID, "turn-1")
	}
}

func TestTurnCompletedSuccess(t *testing.T) {
	params := json.RawMessage(`{"turn":{"id":"turn-1","status":"completed"}}`)
	events := ClassifyNotification(testThread, "turn/completed", params)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventTurnComplete {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventTurnComplete)
	}
	if events[0].TurnID != "turn-1" {
		t.Errorf("turnID: got %q, want %q", events[0].TurnID, "turn-1")
	}
}

func TestTurnCompletedFailed(t *testing.T) {
	params := json.RawMessage(`{"turn":{"id":"turn-1","status":"failed","error":{"message":"model error"}}}`)
	events := ClassifyNotification(testThread, "turn/completed", params)

	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Kind != provider.EventError {
		t.Errorf("first event kind: got %q, want %q", events[0].Kind, provider.EventError)
	}
	if events[0].Content != "model error" {
		t.Errorf("error content: got %q, want %q", events[0].Content, "model error")
	}
	if events[1].Kind != provider.EventTurnComplete {
		t.Errorf("second event kind: got %q, want %q", events[1].Kind, provider.EventTurnComplete)
	}
}

func TestItemAgentMessageDelta(t *testing.T) {
	params := json.RawMessage(`{"delta":"Hello "}`)
	events := ClassifyNotification(testThread, "item/agentMessage/delta", params)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventTextDelta {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventTextDelta)
	}
	if events[0].Content != "Hello " {
		t.Errorf("content: got %q, want %q", events[0].Content, "Hello ")
	}
	if events[0].Role != "assistant" {
		t.Errorf("role: got %q, want %q", events[0].Role, "assistant")
	}
}

func TestItemAgentMessageDeltaEmpty(t *testing.T) {
	params := json.RawMessage(`{"delta":""}`)
	events := ClassifyNotification(testThread, "item/agentMessage/delta", params)

	if len(events) != 0 {
		t.Errorf("expected 0 events for empty delta, got %d", len(events))
	}
}

func TestItemStarted(t *testing.T) {
	params := json.RawMessage(`{"item":{"id":"item-1","type":"command_execution"}}`)
	events := ClassifyNotification(testThread, "item/started", params)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventToolStart {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventToolStart)
	}
	if events[0].ItemID != "item-1" {
		t.Errorf("itemID: got %q, want %q", events[0].ItemID, "item-1")
	}
	if events[0].ItemType != "command_execution" {
		t.Errorf("itemType: got %q, want %q", events[0].ItemType, "command_execution")
	}
}

func TestItemCompleted(t *testing.T) {
	params := json.RawMessage(`{"item":{"id":"item-1","type":"command_execution"}}`)
	events := ClassifyNotification(testThread, "item/completed", params)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventToolComplete {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventToolComplete)
	}
}

func TestCommandExecutionOutputDelta(t *testing.T) {
	params := json.RawMessage(`{"delta":"output line\n"}`)
	events := ClassifyNotification(testThread, "item/commandExecution/outputDelta", params)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventCommandOutput {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventCommandOutput)
	}
	if events[0].Content != "output line\n" {
		t.Errorf("content: got %q, want %q", events[0].Content, "output line\n")
	}
}

func TestTurnDiffUpdated(t *testing.T) {
	params := json.RawMessage(`{"diff":"--- a/main.go\n+++ b/main.go\n"}`)
	events := ClassifyNotification(testThread, "turn/diff/updated", params)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventDiff {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventDiff)
	}
	if events[0].Content != "--- a/main.go\n+++ b/main.go\n" {
		t.Errorf("content: got %q", events[0].Content)
	}
}

func TestFileChangeOutputDelta(t *testing.T) {
	params := json.RawMessage(`{"delta":"diff content"}`)
	events := ClassifyNotification(testThread, "item/fileChange/outputDelta", params)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventDiff {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventDiff)
	}
}

func TestTokenUsageUpdated(t *testing.T) {
	params := json.RawMessage(`{"inputTokens":100,"outputTokens":50}`)
	events := ClassifyNotification(testThread, "thread/tokenUsage/updated", params)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventTokenUsage {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventTokenUsage)
	}
}

func TestErrorNotification(t *testing.T) {
	params := json.RawMessage(`{"error":{"message":"rate limited"}}`)
	events := ClassifyNotification(testThread, "error", params)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventError {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventError)
	}
	if events[0].Content != "rate limited" {
		t.Errorf("content: got %q, want %q", events[0].Content, "rate limited")
	}
}

func TestTurnPlanUpdated(t *testing.T) {
	params := json.RawMessage(`{"plan":"step 1, step 2"}`)
	events := ClassifyNotification(testThread, "turn/plan/updated", params)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventSessionStatus {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventSessionStatus)
	}
	if events[0].Content != "plan_updated" {
		t.Errorf("content: got %q, want %q", events[0].Content, "plan_updated")
	}
}

func TestSkippedMethods(t *testing.T) {
	skipped := []string{
		"thread/started",
		"thread/status/changed",
		"thread/name/updated",
		"thread/archived",
		"thread/unarchived",
		"thread/closed",
		"thread/compacted",
		"item/autoApprovalReview/started",
		"item/autoApprovalReview/completed",
		"item/reasoning/textDelta",
		"item/reasoning/summaryTextDelta",
		"item/reasoning/summaryPartAdded",
		"item/mcpToolCall/progress",
		"serverRequest/resolved",
		"account/updated",
		"account/rateLimits/updated",
		"account/login/completed",
		"model/rerouted",
		"configWarning",
		"deprecationNotice",
	}

	for _, method := range skipped {
		events := ClassifyNotification(testThread, method, json.RawMessage(`{}`))
		if len(events) != 0 {
			t.Errorf("method %q: expected 0 events, got %d", method, len(events))
		}
	}
}

func TestUnknownMethod(t *testing.T) {
	events := ClassifyNotification(testThread, "future/feature", json.RawMessage(`{}`))
	if len(events) != 0 {
		t.Errorf("expected 0 events for unknown method, got %d", len(events))
	}
}

func TestThreadIDPassthrough(t *testing.T) {
	params := json.RawMessage(`{"turn":{"id":"t1"}}`)
	events := ClassifyNotification("my-thread-123", "turn/started", params)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].ThreadID != "my-thread-123" {
		t.Errorf("threadID: got %q, want %q", events[0].ThreadID, "my-thread-123")
	}
}

// -- Session unit tests --

func TestWriteNotificationFormat(t *testing.T) {
	msg := map[string]any{
		"jsonrpc": "2.0",
		"method":  "initialized",
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed map[string]json.RawMessage
	json.Unmarshal(data, &parsed)

	var jsonrpc string
	json.Unmarshal(parsed["jsonrpc"], &jsonrpc)
	if jsonrpc != "2.0" {
		t.Errorf("jsonrpc: got %q, want %q", jsonrpc, "2.0")
	}

	var method string
	json.Unmarshal(parsed["method"], &method)
	if method != "initialized" {
		t.Errorf("method: got %q, want %q", method, "initialized")
	}

	// Notifications should not have an id.
	if _, ok := parsed["id"]; ok {
		t.Error("notification should not have id")
	}
}

func TestSendTurnStartFormat(t *testing.T) {
	params := map[string]any{
		"threadId": "codex-thread-123",
		"input": []map[string]any{{
			"type":          "text",
			"text":          "hello",
			"text_elements": []any{},
		}},
	}

	data, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed map[string]json.RawMessage
	json.Unmarshal(data, &parsed)

	var threadID string
	json.Unmarshal(parsed["threadId"], &threadID)
	if threadID != "codex-thread-123" {
		t.Errorf("threadId: got %q, want %q", threadID, "codex-thread-123")
	}

	var input []struct {
		Type         string `json:"type"`
		Text         string `json:"text"`
		TextElements []any  `json:"text_elements"`
	}
	json.Unmarshal(parsed["input"], &input)
	if len(input) != 1 {
		t.Fatalf("input: expected 1 item, got %d", len(input))
	}
	if input[0].Type != "text" {
		t.Errorf("input type: got %q, want %q", input[0].Type, "text")
	}
	if input[0].Text != "hello" {
		t.Errorf("input text: got %q, want %q", input[0].Text, "hello")
	}
}

func TestRespondToApprovalAccept(t *testing.T) {
	msg := map[string]any{
		"jsonrpc": "2.0",
		"id":      int64(42),
		"result":  map[string]any{"decision": "accept"},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed map[string]json.RawMessage
	json.Unmarshal(data, &parsed)

	var result struct {
		Decision string `json:"decision"`
	}
	json.Unmarshal(parsed["result"], &result)
	if result.Decision != "accept" {
		t.Errorf("decision: got %q, want %q", result.Decision, "accept")
	}
}

func TestRespondToApprovalDecline(t *testing.T) {
	msg := map[string]any{
		"jsonrpc": "2.0",
		"id":      int64(42),
		"result":  map[string]any{"decision": "decline"},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed map[string]json.RawMessage
	json.Unmarshal(data, &parsed)

	var result struct {
		Decision string `json:"decision"`
	}
	json.Unmarshal(parsed["result"], &result)
	if result.Decision != "decline" {
		t.Errorf("decision: got %q, want %q", result.Decision, "decline")
	}
}

func TestBuildThreadParams(t *testing.T) {
	cfg := Config{
		Model:          "gpt-4.1",
		Sandbox:        "workspace-write",
		ApprovalPolicy: "on-request",
		SystemPrompt:   "Be helpful",
	}

	params := buildThreadParams(cfg)

	if params["model"] != "gpt-4.1" {
		t.Errorf("model: got %v, want %q", params["model"], "gpt-4.1")
	}
	if params["sandboxPolicy"] != "workspace" {
		t.Errorf("sandboxPolicy: got %v, want %q", params["sandboxPolicy"], "workspace")
	}
	if params["approvalPolicy"] != "on-request" {
		t.Errorf("approvalPolicy: got %v, want %q", params["approvalPolicy"], "on-request")
	}
	if params["baseInstructions"] != "Be helpful" {
		t.Errorf("baseInstructions: got %v, want %q", params["baseInstructions"], "Be helpful")
	}
}

func TestBuildThreadParamsDangerMode(t *testing.T) {
	cfg := Config{Sandbox: "danger-full-access"}
	params := buildThreadParams(cfg)

	if params["approvalPolicy"] != "never" {
		t.Errorf("approvalPolicy: got %v, want %q", params["approvalPolicy"], "never")
	}
	if params["sandboxPolicy"] != "none" {
		t.Errorf("sandboxPolicy: got %v, want %q", params["sandboxPolicy"], "none")
	}
}

func TestBuildApprovalMetaCommand(t *testing.T) {
	params := json.RawMessage(`{"command":"ls -la"}`)
	meta := buildApprovalMeta("t1", "item/commandExecution/requestApproval", 42, params)

	var approval provider.ApprovalRequest
	json.Unmarshal(meta, &approval)

	if approval.RequestID != "42" {
		t.Errorf("requestID: got %q, want %q", approval.RequestID, "42")
	}
	if approval.ToolName != "command" {
		t.Errorf("toolName: got %q, want %q", approval.ToolName, "command")
	}
	if approval.Title != "Run command" {
		t.Errorf("title: got %q, want %q", approval.Title, "Run command")
	}
}

func TestBuildApprovalMetaFileChange(t *testing.T) {
	params := json.RawMessage(`{"filePath":"/tmp/test.go"}`)
	meta := buildApprovalMeta("t1", "item/fileChange/requestApproval", 99, params)

	var approval provider.ApprovalRequest
	json.Unmarshal(meta, &approval)

	if approval.ToolName != "file_change" {
		t.Errorf("toolName: got %q, want %q", approval.ToolName, "file_change")
	}
	if approval.Description != "/tmp/test.go" {
		t.Errorf("description: got %q, want %q", approval.Description, "/tmp/test.go")
	}
}

func TestReadStringFromResponse(t *testing.T) {
	data := json.RawMessage(`{"result":{"thread":{"id":"thread-abc"}}}`)

	got := readStringFromResponse(data, "result", "thread", "id")
	if got != "thread-abc" {
		t.Errorf("got %q, want %q", got, "thread-abc")
	}
}

func TestReadStringFromResponseMissingKey(t *testing.T) {
	data := json.RawMessage(`{"result":{}}`)
	got := readStringFromResponse(data, "result", "thread", "id")
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestDispatchLineResponse(t *testing.T) {
	// Create a session with a pending request.
	s := &Session{
		pending: make(map[int64]chan json.RawMessage),
	}

	ch := make(chan json.RawMessage, 1)
	s.pending[1] = ch

	// Dispatch a response.
	line := []byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	s.dispatchLine(line)

	select {
	case resp := <-ch:
		if resp == nil {
			t.Fatal("expected non-nil response")
		}
	default:
		t.Fatal("expected response to be routed to pending channel")
	}
}

func TestDispatchLineNotification(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "t1",
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	line := []byte(`{"jsonrpc":"2.0","method":"turn/started","params":{"turn":{"id":"turn-1"}}}`)
	s.dispatchLine(line)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventTurnStart {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventTurnStart)
	}
}
