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
