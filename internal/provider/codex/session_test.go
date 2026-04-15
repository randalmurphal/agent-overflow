package codex

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

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
	if !events[0].Replace {
		t.Fatal("expected turn/diff/updated to mark replace=true")
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

func TestClassifyReasoningTextDelta(t *testing.T) {
	params := json.RawMessage(`{"delta":"thinking about this..."}`)
	events := ClassifyNotification(testThread, "item/reasoning/textDelta", params)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventThinking {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventThinking)
	}
	if events[0].Content != "thinking about this..." {
		t.Errorf("content: got %q, want %q", events[0].Content, "thinking about this...")
	}
	if events[0].ThreadID != testThread {
		t.Errorf("threadID: got %q, want %q", events[0].ThreadID, testThread)
	}
}

func TestClassifyReasoningTextDeltaFallbackKeys(t *testing.T) {
	// Falls back to "text" key when "delta" is missing.
	params := json.RawMessage(`{"text":"via text key"}`)
	events := ClassifyNotification(testThread, "item/reasoning/textDelta", params)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Content != "via text key" {
		t.Errorf("content: got %q, want %q", events[0].Content, "via text key")
	}

	// Falls back to "content.text" when both "delta" and "text" are missing.
	params2 := json.RawMessage(`{"content":{"text":"nested fallback"}}`)
	events2 := ClassifyNotification(testThread, "item/reasoning/textDelta", params2)

	if len(events2) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events2))
	}
	if events2[0].Content != "nested fallback" {
		t.Errorf("content: got %q, want %q", events2[0].Content, "nested fallback")
	}

	// Returns nil when all keys are missing.
	params3 := json.RawMessage(`{"other":"value"}`)
	events3 := ClassifyNotification(testThread, "item/reasoning/textDelta", params3)
	if len(events3) != 0 {
		t.Errorf("expected 0 events for empty delta, got %d", len(events3))
	}
}

func TestClassifyReasoningSummaryTextDelta(t *testing.T) {
	params := json.RawMessage(`{"delta":"summarizing..."}`)
	events := ClassifyNotification(testThread, "item/reasoning/summaryTextDelta", params)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventThinking {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventThinking)
	}
	if events[0].Content != "summarizing..." {
		t.Errorf("content: got %q, want %q", events[0].Content, "summarizing...")
	}
}

func TestClassifyThreadNameUpdated(t *testing.T) {
	params := json.RawMessage(`{"threadName":"My New Thread"}`)
	events := ClassifyNotification(testThread, "thread/name/updated", params)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventThreadRenamed {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventThreadRenamed)
	}
	if events[0].Content != "My New Thread" {
		t.Errorf("content: got %q, want %q", events[0].Content, "My New Thread")
	}

	var meta map[string]string
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["newTitle"] != "My New Thread" {
		t.Errorf("meta newTitle: got %q, want %q", meta["newTitle"], "My New Thread")
	}
}

func TestClassifyThreadNameUpdatedFallback(t *testing.T) {
	// Falls back to "name" key when "threadName" is missing.
	params := json.RawMessage(`{"name":"Fallback Name"}`)
	events := ClassifyNotification(testThread, "thread/name/updated", params)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Content != "Fallback Name" {
		t.Errorf("content: got %q, want %q", events[0].Content, "Fallback Name")
	}
}

func TestClassifyRateLimitsUpdated(t *testing.T) {
	params := json.RawMessage(`{
		"rateLimits": {
			"limitId": "codex",
			"limitName": "Codex",
			"primary": {"usedPercent": 5, "windowDurationMins": 300, "resetsAt": 1775803864},
			"secondary": {"usedPercent": 3, "windowDurationMins": 10080, "resetsAt": 1776372636}
		},
		"rateLimitsByLimitId": {
			"codex": {
				"limitId": "codex",
				"limitName": "Codex",
				"primary": {"usedPercent": 5, "windowDurationMins": 300, "resetsAt": 1775803864},
				"secondary": {"usedPercent": 3, "windowDurationMins": 10080, "resetsAt": 1776372636}
			},
			"spark": {
				"limitId": "spark",
				"limitName": "GPT-5.3-Codex-Spark",
				"primary": {"usedPercent": 0, "windowDurationMins": 300, "resetsAt": 1775809666},
				"secondary": {"usedPercent": 0, "windowDurationMins": 10080, "resetsAt": 1776396466}
			}
		}
	}`)
	events := ClassifyNotification(testThread, "account/rateLimits/updated", params)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventRateLimits {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventRateLimits)
	}
	if events[0].ThreadID != testThread {
		t.Errorf("threadID: got %q, want %q", events[0].ThreadID, testThread)
	}

	var snapshot provider.RateLimitsSnapshot
	if err := json.Unmarshal(events[0].Meta, &snapshot); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if snapshot.Provider != string(provider.Codex) {
		t.Errorf("provider: got %q, want %q", snapshot.Provider, provider.Codex)
	}
	if snapshot.UpdatedAt == 0 {
		t.Fatal("expected UpdatedAt to be populated")
	}
	if len(snapshot.Limits) != 4 {
		t.Fatalf("limits len: got %d, want 4", len(snapshot.Limits))
	}
	if snapshot.Limits[0].LimitID != "codex" || snapshot.Limits[0].WindowMins != 300 {
		t.Errorf("limits[0]: got %+v", snapshot.Limits[0])
	}
	if snapshot.Limits[1].LimitID != "codex" || snapshot.Limits[1].WindowMins != 10080 {
		t.Errorf("limits[1]: got %+v", snapshot.Limits[1])
	}
	if snapshot.Limits[2].LimitID != "spark" || snapshot.Limits[2].WindowMins != 300 {
		t.Errorf("limits[2]: got %+v", snapshot.Limits[2])
	}
	if snapshot.Limits[3].LimitID != "spark" || snapshot.Limits[3].WindowMins != 10080 {
		t.Errorf("limits[3]: got %+v", snapshot.Limits[3])
	}
}

func TestClassifyModelRerouted(t *testing.T) {
	params := json.RawMessage(`{"toModel":"gpt-4.1-mini"}`)
	events := ClassifyNotification(testThread, "model/rerouted", params)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventModelRerouted {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventModelRerouted)
	}
	if events[0].Content != "gpt-4.1-mini" {
		t.Errorf("content: got %q, want %q", events[0].Content, "gpt-4.1-mini")
	}

	var meta map[string]string
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["newModel"] != "gpt-4.1-mini" {
		t.Errorf("meta newModel: got %q, want %q", meta["newModel"], "gpt-4.1-mini")
	}
}

func TestClassifyThreadCompacted(t *testing.T) {
	params := json.RawMessage(`{"compactionId":"c1","tokensRemoved":500}`)
	events := ClassifyNotification(testThread, "thread/compacted", params)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventCompactBoundary {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventCompactBoundary)
	}
	if events[0].ThreadID != testThread {
		t.Errorf("threadID: got %q, want %q", events[0].ThreadID, testThread)
	}
	if string(events[0].Meta) != string(params) {
		t.Errorf("meta: got %s, want %s", string(events[0].Meta), string(params))
	}
}

func TestSkippedMethods(t *testing.T) {
	skipped := []string{
		"thread/started",
		"thread/status/changed",
		"thread/archived",
		"thread/unarchived",
		"thread/closed",
		"item/autoApprovalReview/started",
		"item/autoApprovalReview/completed",
		"item/reasoning/summaryPartAdded",
		"item/mcpToolCall/progress",
		"serverRequest/resolved",
		"account/updated",
		"account/login/completed",
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
	meta := buildApprovalMeta("t1", "", "item/commandExecution/requestApproval", 42, params)

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
	meta := buildApprovalMeta("t1", "", "item/fileChange/requestApproval", 99, params)

	var approval provider.ApprovalRequest
	json.Unmarshal(meta, &approval)

	if approval.ToolName != "file_change" {
		t.Errorf("toolName: got %q, want %q", approval.ToolName, "file_change")
	}
	if approval.Description != "/tmp/test.go" {
		t.Errorf("description: got %q, want %q", approval.Description, "/tmp/test.go")
	}
}

func TestReadRouteFields(t *testing.T) {
	params := json.RawMessage(`{"turn":{"id":"turn-7"},"item":{"id":"item-4"}}`)
	turnID, itemID := readRouteFields(params)

	if turnID != "turn-7" {
		t.Errorf("turnID: got %q, want %q", turnID, "turn-7")
	}
	if itemID != "item-4" {
		t.Errorf("itemID: got %q, want %q", itemID, "item-4")
	}
}

func TestReadRouteFieldsTopLevelFallback(t *testing.T) {
	params := json.RawMessage(`{"turnId":"turn-9","itemId":"item-2"}`)
	turnID, itemID := readRouteFields(params)

	if turnID != "turn-9" {
		t.Errorf("turnID: got %q, want %q", turnID, "turn-9")
	}
	if itemID != "item-2" {
		t.Errorf("itemID: got %q, want %q", itemID, "item-2")
	}
}

func TestBuildUserInputMeta(t *testing.T) {
	params := json.RawMessage(`{"turn":{"id":"turn-2"},"questions":[{"id":"sandbox_mode","header":"Sandbox","question":"Which mode should be used?","options":[{"label":"workspace-write","description":"Allow workspace writes only"}],"multiSelect":true}]}`)
	meta := buildUserInputMeta("t1", "turn-2", 42, params)

	var approval provider.ApprovalRequest
	if err := json.Unmarshal(meta, &approval); err != nil {
		t.Fatalf("unmarshal approval: %v", err)
	}

	if approval.Kind != "user-input" {
		t.Errorf("kind: got %q, want %q", approval.Kind, "user-input")
	}
	if approval.TurnID != "turn-2" {
		t.Errorf("turnID: got %q, want %q", approval.TurnID, "turn-2")
	}
	if approval.ToolName != "user_input" {
		t.Errorf("toolName: got %q, want %q", approval.ToolName, "user_input")
	}
	if len(approval.Questions) != 1 {
		t.Fatalf("questions len: got %d, want 1", len(approval.Questions))
	}
	if !approval.Questions[0].MultiSelect {
		t.Fatal("expected multiSelect=true")
	}
	if string(approval.Input) != string(params) {
		t.Errorf("input: got %s, want %s", approval.Input, params)
	}
}

func TestBuildPermissionMeta(t *testing.T) {
	params := json.RawMessage(`{"turnId":"turn-5","reason":"Need broader write access","permissions":{"network":{"enabled":true},"fileSystem":{"read":["/tmp/project/src"],"write":["/tmp/project/out"]}}}`)
	meta := buildPermissionMeta("t1", "turn-5", 77, params)

	var approval provider.ApprovalRequest
	if err := json.Unmarshal(meta, &approval); err != nil {
		t.Fatalf("unmarshal approval: %v", err)
	}

	if approval.Kind != "permission" {
		t.Errorf("kind: got %q, want %q", approval.Kind, "permission")
	}
	if approval.Description != "Need broader write access" {
		t.Errorf("description: got %q, want %q", approval.Description, "Need broader write access")
	}
	if approval.Permissions == nil || approval.Permissions.Network == nil || approval.Permissions.FileSystem == nil {
		t.Fatal("expected permission profile to be populated")
	}
	if approval.Permissions.Network.Enabled == nil || !*approval.Permissions.Network.Enabled {
		t.Fatal("expected network enabled=true")
	}
	if got := approval.Permissions.FileSystem.Write[0]; got != "/tmp/project/out" {
		t.Errorf("fileSystem.write[0]: got %q, want %q", got, "/tmp/project/out")
	}
	if string(approval.Input) != string(params) {
		t.Errorf("input: got %s, want %s", approval.Input, params)
	}
}

func TestReadStringFromResponse(t *testing.T) {
	// sendRequest returns just the result payload, not the full JSON-RPC envelope.
	data := json.RawMessage(`{"thread":{"id":"thread-abc"}}`)
	got := readStringFromResponse(data, "thread", "id")
	if got != "thread-abc" {
		t.Errorf("got %q, want %q", got, "thread-abc")
	}
}

func TestReadStringFromResponseMissingKey(t *testing.T) {
	data := json.RawMessage(`{}`)
	got := readStringFromResponse(data, "thread", "id")
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestReadStringFromResponseInvalidJSON(t *testing.T) {
	got := readStringFromResponse(json.RawMessage(`not json`), "key")
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestReadStringFromResponseNonStringValue(t *testing.T) {
	data := json.RawMessage(`{"count": 42}`)
	got := readStringFromResponse(data, "count")
	if got != "" {
		t.Errorf("got %q, want empty for non-string value", got)
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

func TestDispatchLineInvalidJSON(t *testing.T) {
	s := &Session{
		pending: make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {},
	}
	// Should not panic — logs and returns.
	s.dispatchLine([]byte(`not valid json`))
}

func TestDispatchLineResponseNonIntegerID(t *testing.T) {
	s := &Session{
		pending: make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {},
	}
	// Float ID — Int64() fails, logged and returned.
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","id":1.5,"result":{}}`))
}

func TestDispatchLineResponseNoMatchingPending(t *testing.T) {
	s := &Session{
		pending: make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {},
	}
	// Valid response but no pending channel for id=999 — silently ignored.
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","id":999,"result":{}}`))
}

func TestDispatchLineServerRequest(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "t1",
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	// Server request with id + method — routes to handleServerRequest.
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: "cat"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer func() {
		cancel()
		proc.Close()
	}()
	s.proc = proc

	line := []byte(`{"jsonrpc":"2.0","id":1,"method":"item/commandExecution/requestApproval","params":{"command":"ls"}}`)
	s.dispatchLine(line)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventApprovalRequest {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventApprovalRequest)
	}
}

func TestDispatchLineServerRequestNonIntegerID(t *testing.T) {
	s := &Session{
		pending: make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {},
	}
	// Server request with float ID — logged and returned.
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","id":1.5,"method":"item/commandExecution/requestApproval","params":{}}`))
}

// -- Session lifecycle tests using cat subprocess --

// newTestCodexSession creates a Session backed by `cat`, which echoes
// stdin to stdout. readLoop is started, enabling end-to-end testing of
// dispatch, notifications, responses, and server requests.
func newTestCodexSession(t *testing.T) (*Session, <-chan provider.ProviderEvent) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: "cat"})
	if err != nil {
		t.Fatalf("spawn cat: %v", err)
	}
	eventCh := make(chan provider.ProviderEvent, 100)
	s := &Session{
		proc:          proc,
		threadID:      testThread,
		codexThreadID: "codex-thread-1",
		pending:       make(map[int64]chan json.RawMessage),
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

func codexWaitEvent(t *testing.T, ch <-chan provider.ProviderEvent) provider.ProviderEvent {
	t.Helper()
	select {
	case evt := <-ch:
		return evt
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for event")
		return provider.ProviderEvent{}
	}
}

func TestCodexWriteNotification(t *testing.T) {
	s, _ := newTestCodexSession(t)

	if err := s.writeNotification("initialized", nil); err != nil {
		t.Fatalf("writeNotification: %v", err)
	}
	if err := s.writeNotification("test/method", map[string]any{"key": "value"}); err != nil {
		t.Fatalf("writeNotification with params: %v", err)
	}
}

func TestCodexWriteResponse(t *testing.T) {
	s, _ := newTestCodexSession(t)

	if err := s.writeResponse(42, map[string]any{"ok": true}); err != nil {
		t.Fatalf("writeResponse: %v", err)
	}
}

func TestCodexRespondToApprovalMethod(t *testing.T) {
	s, _ := newTestCodexSession(t)

	tests := []struct {
		name     string
		decision string
	}{
		{"allow", "allow"},
		{"deny", "deny"},
		{"allow_session", "allow_session"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := s.RespondToApproval(context.Background(), 42, tt.decision)
			if err != nil {
				t.Fatalf("RespondToApproval(%s): %v", tt.decision, err)
			}
		})
	}
}

func TestCodexThreadIDAccessor(t *testing.T) {
	s, _ := newTestCodexSession(t)
	if got := s.ThreadID(); got != testThread {
		t.Errorf("ThreadID: got %q, want %q", got, testThread)
	}
}

func TestCodexReadLoopDispatchesNotification(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	// Write a turn/started notification through cat.
	line := []byte(`{"jsonrpc":"2.0","method":"turn/started","params":{"turn":{"id":"turn-1"}}}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}

	evt := codexWaitEvent(t, eventCh)
	if evt.Kind != provider.EventTurnStart {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventTurnStart)
	}
	if evt.TurnID != "turn-1" {
		t.Errorf("turnID: got %q, want %q", evt.TurnID, "turn-1")
	}
}

func TestCodexReadLoopRoutesResponseToPending(t *testing.T) {
	s, _ := newTestCodexSession(t)

	// Set up a pending request.
	ch := make(chan json.RawMessage, 1)
	s.mu.Lock()
	s.pending[42] = ch
	s.mu.Unlock()

	// Write a response with id=42 through cat.
	respLine := []byte(`{"jsonrpc":"2.0","id":42,"result":{"ok":true}}`)
	if err := s.proc.WriteLine(respLine); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case resp := <-ch:
		if resp == nil {
			t.Fatal("expected non-nil response")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for response on pending channel")
	}

	s.mu.Lock()
	delete(s.pending, 42)
	s.mu.Unlock()
}

func TestCodexHandleServerRequestApproval(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	// Write an approval server request through cat.
	line := []byte(`{"jsonrpc":"2.0","id":1,"method":"item/commandExecution/requestApproval","params":{"command":"rm -rf /"}}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}

	evt := codexWaitEvent(t, eventCh)
	if evt.Kind != provider.EventApprovalRequest {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventApprovalRequest)
	}

	var approval provider.ApprovalRequest
	if err := json.Unmarshal(evt.Meta, &approval); err != nil {
		t.Fatalf("unmarshal approval: %v", err)
	}
	if approval.RequestID != "1" {
		t.Errorf("requestID: got %q, want %q", approval.RequestID, "1")
	}
	if approval.ToolName != "command" {
		t.Errorf("toolName: got %q, want %q", approval.ToolName, "command")
	}
}

func TestCodexHandleServerRequestFileApproval(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	line := []byte(`{"jsonrpc":"2.0","id":2,"method":"item/fileChange/requestApproval","params":{"filePath":"/tmp/test.go"}}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}

	evt := codexWaitEvent(t, eventCh)
	if evt.Kind != provider.EventApprovalRequest {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventApprovalRequest)
	}
}

func TestCodexHandleServerRequestUserInput(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	line := []byte(`{"jsonrpc":"2.0","id":3,"method":"item/tool/requestUserInput","params":{"turn":{"id":"turn-3"},"item":{"id":"item-8"},"questions":[{"id":"scope","header":"Scope","question":"Choose a scope","options":[{"label":"turn","description":"Apply only to this turn"},{"label":"session","description":"Apply for the whole session"}],"multiSelect":false}]}}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}

	evt := codexWaitEvent(t, eventCh)
	if evt.Kind != provider.EventApprovalRequest {
		t.Fatalf("kind: got %q, want %q", evt.Kind, provider.EventApprovalRequest)
	}
	if evt.TurnID != "turn-3" {
		t.Errorf("turnID: got %q, want %q", evt.TurnID, "turn-3")
	}
	if evt.ItemID != "item-8" {
		t.Errorf("itemID: got %q, want %q", evt.ItemID, "item-8")
	}

	var approval provider.ApprovalRequest
	if err := json.Unmarshal(evt.Meta, &approval); err != nil {
		t.Fatalf("unmarshal approval: %v", err)
	}
	if approval.Kind != "user-input" {
		t.Errorf("kind: got %q, want %q", approval.Kind, "user-input")
	}
	if len(approval.Questions) != 1 {
		t.Fatalf("questions len: got %d, want 1", len(approval.Questions))
	}
	if approval.Questions[0].ID != "scope" {
		t.Errorf("question id: got %q, want %q", approval.Questions[0].ID, "scope")
	}
}

func TestCodexHandleServerRequestPermission(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	line := []byte(`{"jsonrpc":"2.0","id":4,"method":"item/permissions/requestApproval","params":{"turnId":"turn-4","itemId":"item-9","reason":"Need broader write access","permissions":{"network":{"enabled":true},"fileSystem":{"read":["/tmp/project/src"],"write":["/tmp/project/out"]}}}}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}

	evt := codexWaitEvent(t, eventCh)
	if evt.Kind != provider.EventApprovalRequest {
		t.Fatalf("kind: got %q, want %q", evt.Kind, provider.EventApprovalRequest)
	}
	if evt.TurnID != "turn-4" {
		t.Errorf("turnID: got %q, want %q", evt.TurnID, "turn-4")
	}
	if evt.ItemID != "item-9" {
		t.Errorf("itemID: got %q, want %q", evt.ItemID, "item-9")
	}

	var approval provider.ApprovalRequest
	if err := json.Unmarshal(evt.Meta, &approval); err != nil {
		t.Fatalf("unmarshal approval: %v", err)
	}
	if approval.Kind != "permission" {
		t.Errorf("kind: got %q, want %q", approval.Kind, "permission")
	}
	if approval.Permissions == nil || approval.Permissions.FileSystem == nil {
		t.Fatal("expected filesystem permissions to be populated")
	}
	if approval.Permissions.FileSystem.Read[0] != "/tmp/project/src" {
		t.Errorf("fileSystem.read[0]: got %q, want %q", approval.Permissions.FileSystem.Read[0], "/tmp/project/src")
	}
}

func TestCodexHandleServerRequestUnknown(t *testing.T) {
	s, _ := newTestCodexSession(t)

	// Unknown server request — should send error response.
	// With cat, the error response echoes back. We just verify no crash.
	line := []byte(`{"jsonrpc":"2.0","id":1,"method":"unknown/request","params":{}}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Give readLoop time to process both the original and echo.
	time.Sleep(200 * time.Millisecond)
}

func TestCodexSendRequestViaCat(t *testing.T) {
	s, _ := newTestCodexSession(t)

	// With cat: request echoes -> dispatchLine sees server request (id + method) ->
	// handleServerRequest sends JSON-RPC error (unknown method) -> error echoes ->
	// dispatchLine sees response -> routes to pending -> sendRequest receives it.
	// After the handleServerRequest fix, error is at top level, so sendRequest returns error.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := s.sendRequest(ctx, "test/method", map[string]any{"key": "value"})
	if err == nil {
		t.Fatal("expected error from sendRequest (unknown method)")
	}
}

func TestCodexSendRequestContextCancel(t *testing.T) {
	s, _ := newTestCodexSession(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := s.sendRequest(ctx, "test/method", nil)
	if err == nil {
		t.Fatal("expected context canceled error")
	}
}

func TestCodexSend(t *testing.T) {
	s, _ := newTestCodexSession(t)

	// Send calls sendRequest("turn/start"). With cat, this goes through the
	// echo cycle and returns an error (unknown method). Send propagates it.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := s.Send(ctx, "hello world")
	// Expected to fail because cat echo + handleServerRequest produces error response.
	if err == nil {
		t.Fatal("expected error from Send via cat echo")
	}
}

func TestCodexInterruptNoActiveTurn(t *testing.T) {
	s, _ := newTestCodexSession(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := s.Interrupt(ctx)
	if err == nil {
		t.Fatal("expected error when no active turn")
	}
}

func TestCodexInterruptWithActiveTurn(t *testing.T) {
	s, _ := newTestCodexSession(t)

	// Simulate turn/started by setting activeTurnID.
	s.mu.Lock()
	s.activeTurnID = "turn-1"
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := s.Interrupt(ctx)
	// Expected to fail because cat echo + handleServerRequest produces error response,
	// but it should NOT fail with "no active turn".
	if err != nil && err.Error() == "codex: no active turn to interrupt" {
		t.Fatal("should have attempted RPC, not returned no-active-turn error")
	}
}

func TestCodexReadLoopEmitsDisconnectedOnExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: "cat"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	eventCh := make(chan provider.ProviderEvent, 100)
	s := &Session{
		proc:          proc,
		threadID:      testThread,
		codexThreadID: "test",
		pending:       make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			eventCh <- evt
		},
		cancel: cancel,
	}
	go s.readLoop()

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

func TestCodexReadLoopCleansPendingOnExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: "cat"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	eventCh := make(chan provider.ProviderEvent, 100)
	s := &Session{
		proc:          proc,
		threadID:      testThread,
		codexThreadID: "test",
		pending:       make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			eventCh <- evt
		},
		cancel: cancel,
	}

	// Add a pending request before readLoop starts.
	pendingCh := make(chan json.RawMessage, 1)
	s.mu.Lock()
	s.pending[99] = pendingCh
	s.mu.Unlock()

	go s.readLoop()

	// Kill the process — readLoop should clean up pending.
	s.Close()

	// The pending channel should be closed.
	select {
	case _, ok := <-pendingCh:
		if ok {
			t.Error("expected pending channel to be closed, got a value")
		}
		// Channel was closed — correct.
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for pending channel to be closed")
	}
}

func TestCodexNewSessionWithMock(t *testing.T) {
	// Create a mock Codex app-server script that responds to JSON-RPC requests.
	script := `#!/bin/bash
while IFS= read -r line; do
    id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | grep -o '[0-9]*')
    if [ -n "$id" ]; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"thread\":{\"id\":\"mock-thread-123\"}}}"
    fi
done
`
	scriptDir := t.TempDir()
	scriptPath := scriptDir + "/codex"
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write mock script: %v", err)
	}

	ctx := context.Background()
	eventCh := make(chan provider.ProviderEvent, 100)
	s, err := NewSession(ctx, testThread, Config{
		Binary:  scriptPath,
		Model:   "test-model",
		WorkDir: "/tmp",
	}, func(evt provider.ProviderEvent) {
		eventCh <- evt
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer s.Close()

	if s.threadID != testThread {
		t.Errorf("threadID: got %q, want %q", s.threadID, testThread)
	}
	if s.codexThreadID != "mock-thread-123" {
		t.Errorf("codexThreadID: got %q, want %q", s.codexThreadID, "mock-thread-123")
	}

	// EventInit should have been emitted.
	evt := codexWaitEvent(t, eventCh)
	if evt.Kind != provider.EventInit {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventInit)
	}
	var info provider.SessionInfo
	if err := json.Unmarshal(evt.Meta, &info); err != nil {
		t.Fatalf("unmarshal session info: %v", err)
	}
	if info.SessionID != "mock-thread-123" {
		t.Errorf("sessionID: got %q, want %q", info.SessionID, "mock-thread-123")
	}
	if info.Model != "test-model" {
		t.Errorf("model: got %q, want %q", info.Model, "test-model")
	}
}
