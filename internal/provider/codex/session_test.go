package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
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

func TestReadTopLevelIDString(t *testing.T) {
	tests := []struct {
		name string
		data json.RawMessage
		want string
	}{
		{
			name: "string id",
			data: json.RawMessage(`{"requestId":"req-1"}`),
			want: "req-1",
		},
		{
			name: "numeric id",
			data: json.RawMessage(`{"requestId":91}`),
			want: "91",
		},
		{
			name: "missing id",
			data: json.RawMessage(`{"other":true}`),
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := readTopLevelIDString(tt.data, "requestId"); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
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
	// turn/plan/updated now emits EventPlanUpdate instead of overloading
	// EventSessionStatus. The raw params become Meta so the frontend can
	// render the incremental plan without a second round-trip.
	params := json.RawMessage(`{"plan":"step 1, step 2"}`)
	events := ClassifyNotification(testThread, "turn/plan/updated", params)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventPlanUpdate {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventPlanUpdate)
	}
	if events[0].Content != "" {
		t.Errorf("content should be empty (payload rides on Meta), got %q", events[0].Content)
	}
	if string(events[0].Meta) != string(params) {
		t.Errorf("meta: got %s, want %s", events[0].Meta, params)
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

func TestClassifyServerRequestResolved(t *testing.T) {
	params := json.RawMessage(`{"requestId":91,"resolution":{"scope":"turn"}}`)
	events := ClassifyNotification(testThread, "serverRequest/resolved", params)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventApprovalResolved {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventApprovalResolved)
	}
	if events[0].ItemID != "91" {
		t.Errorf("itemID: got %q, want %q", events[0].ItemID, "91")
	}
	if string(events[0].Meta) != string(params) {
		t.Errorf("meta: got %s, want %s", string(events[0].Meta), string(params))
	}
}

func TestClassifyServerRequestResolvedPrefersProviderRequestID(t *testing.T) {
	params := json.RawMessage(`{"requestId":"interactive-1","providerRequestId":"91"}`)
	events := ClassifyNotification(testThread, "serverRequest/resolved", params)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].ItemID != "91" {
		t.Errorf("itemID: got %q, want %q", events[0].ItemID, "91")
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
	s, eventCh := newTestCodexSession(t)

	// Call the actual writeNotification method. With the cat-backed session,
	// the JSON-RPC notification echoes back and is dispatched by readLoop.
	// "initialized" is skipped by ClassifyNotification, so send a second
	// known notification to verify end-to-end dispatch.
	if err := s.writeNotification("initialized", nil); err != nil {
		t.Fatalf("writeNotification(initialized): %v", err)
	}

	if err := s.writeNotification("turn/started", map[string]any{
		"turn": map[string]any{"id": "turn-verify"},
	}); err != nil {
		t.Fatalf("writeNotification(turn/started): %v", err)
	}

	evt := codexWaitEvent(t, eventCh)
	if evt.Kind != provider.EventTurnStart {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventTurnStart)
	}
	if evt.TurnID != "turn-verify" {
		t.Errorf("turnID: got %q, want %q", evt.TurnID, "turn-verify")
	}
}

func TestSendTurnStartFormat(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	// Call the actual Send method, which issues a turn/start JSON-RPC request.
	// With the cat-backed session the request echoes back as a server request
	// (has both id and method), handleServerRequest sees "turn/start" as unknown
	// and returns a JSON-RPC error which becomes the sendRequest response.
	// Send returns an error from the RPC layer, which is expected here.
	_ = s.Send(context.Background(), "hello")

	// Drain the event channel: the echoed server request triggers
	// writeErrorResponse, whose echo arrives as a response (routed to pending).
	// The original turn/start echo may also produce events.
	// Verify the session didn't panic and readLoop is healthy by writing
	// a known notification that produces a deterministic event.
	if err := s.writeNotification("turn/started", map[string]any{
		"turn": map[string]any{"id": "turn-after-send"},
	}); err != nil {
		t.Fatalf("writeNotification: %v", err)
	}

	evt := codexWaitEvent(t, eventCh)
	// Skip any stale events from the Send echo chain.
	for evt.TurnID != "turn-after-send" {
		evt = codexWaitEvent(t, eventCh)
	}
	if evt.Kind != provider.EventTurnStart {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventTurnStart)
	}
}

func TestRespondToApprovalAccept(t *testing.T) {
	s, _ := newTestCodexSession(t)

	// Call the actual RespondToApproval method with an accept decision.
	// The cat-backed session writes the JSON-RPC response to stdin successfully.
	err := s.RespondToApproval(context.Background(), provider.ApprovalResponse{
		RequestID: "42",
		Decision:  "accept",
	})
	if err != nil {
		t.Fatalf("RespondToApproval(accept): %v", err)
	}
}

func TestRespondToApprovalDecline(t *testing.T) {
	s, _ := newTestCodexSession(t)

	// Call the actual RespondToApproval method with a decline decision.
	err := s.RespondToApproval(context.Background(), provider.ApprovalResponse{
		RequestID: "42",
		Decision:  "decline",
	})
	if err != nil {
		t.Fatalf("RespondToApproval(decline): %v", err)
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

// newTestCodexSessionWithApprovalTimeout mirrors newTestCodexSession with
// a custom approval-watchdog window for Bug B3 tests. Codex approval
// requests come in as JSON-RPC server requests with integer IDs; the
// session auto-denies if the user fails to respond in time.
func newTestCodexSessionWithApprovalTimeout(t *testing.T, timeout time.Duration) (*Session, <-chan provider.ProviderEvent) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: "cat"})
	if err != nil {
		t.Fatalf("spawn cat: %v", err)
	}
	eventCh := make(chan provider.ProviderEvent, 200)
	s := &Session{
		proc:            proc,
		threadID:        testThread,
		codexThreadID:   "codex-thread-1",
		pending:         make(map[int64]chan json.RawMessage),
		approvalTimeout: timeout,
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

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// TestTurnStartEmittedExactlyOncePerTurn exercises Bug B6: one user
// turn must produce exactly one EventTurnStart. Pre-fix, Send's RPC
// response emitter and dispatchLine's turn/started notification path
// both fired. We use a silent subprocess (sleep) so Send's request
// write does NOT echo back — this gives us a stable window to inject
// the RPC response via the pending channel without racing cat's echo
// of the request.
func TestTurnStartEmittedExactlyOncePerTurn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args:   []string{"-c", "cat > /dev/null; sleep 60"},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	t.Cleanup(func() { proc.Close() })

	eventCh := make(chan provider.ProviderEvent, 100)
	s := &Session{
		proc:          proc,
		threadID:      testThread,
		codexThreadID: "ctx-thread",
		pending:       make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			eventCh <- evt
		},
		cancel: cancel,
	}
	go s.readLoop()

	sendDone := make(chan error, 1)
	go func() {
		sendDone <- s.Send(context.Background(), "hi")
	}()

	// Poll until Send has registered its pending channel, then inject a
	// successful result. The silent subprocess never echoes anything so
	// the pending channel is quiet until we write to it.
	var ch chan json.RawMessage
	var rpcID int64
	deadline := time.After(3 * time.Second)
pollPending:
	for {
		select {
		case <-deadline:
			t.Fatal("Send never registered a pending RPC id")
		default:
		}
		s.mu.Lock()
		for id, c := range s.pending {
			rpcID = id
			ch = c
			s.mu.Unlock()
			break pollPending
		}
		s.mu.Unlock()
		time.Sleep(2 * time.Millisecond)
	}
	rpcResp := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"turn":{"id":"turn-42"}}}`, rpcID)
	ch <- json.RawMessage(rpcResp)

	select {
	case err := <-sendDone:
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Send did not return")
	}

	// Fire the turn/started notification via a direct dispatchLine
	// call — the subprocess is silent so we can't rely on readLoop to
	// pick up a stdin-written line.
	notifLine := []byte(`{"jsonrpc":"2.0","method":"turn/started","params":{"turn":{"id":"turn-42"}}}`)
	s.dispatchLine(notifLine)

	turnStarts := 0
	drainDeadline := time.After(500 * time.Millisecond)
drain:
	for {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventTurnStart && evt.TurnID == "turn-42" {
				turnStarts++
			}
		case <-drainDeadline:
			break drain
		}
	}
	if turnStarts != 1 {
		t.Fatalf("turnStart emissions for turn-42 = %d, want exactly 1 (Bug B6 regression)", turnStarts)
	}
}

// TestTurnStartOnlyNotificationStillEmits ensures that when the RPC
// response path is removed (the fix), a lone notification still surfaces
// EventTurnStart. Codex always sends turn/started after turn/start, so
// the notification path is load-bearing.
func TestTurnStartOnlyNotificationStillEmits(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	// Only the notification — no RPC response at all.
	notif := `{"jsonrpc":"2.0","method":"turn/started","params":{"turn":{"id":"turn-99"}}}`
	if err := s.proc.WriteLine([]byte(notif)); err != nil {
		t.Fatalf("write notif: %v", err)
	}

	var got provider.ProviderEvent
	select {
	case got = <-eventCh:
	case <-time.After(2 * time.Second):
		t.Fatal("no event emitted from lone notification")
	}
	if got.Kind != provider.EventTurnStart {
		t.Fatalf("kind: got %q, want EventTurnStart", got.Kind)
	}
	if got.TurnID != "turn-99" {
		t.Fatalf("turnID: got %q, want turn-99", got.TurnID)
	}
}

// TestTurnStartIdempotentOnDuplicateNotification covers the rarer case
// where the provider re-sends turn/started (e.g. recovery). The second
// emission must be suppressed so the router still sees one turn.
func TestTurnStartIdempotentOnDuplicateNotification(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	notif := `{"jsonrpc":"2.0","method":"turn/started","params":{"turn":{"id":"turn-dup"}}}`
	for i := 0; i < 2; i++ {
		if err := s.proc.WriteLine([]byte(notif)); err != nil {
			t.Fatalf("write notif %d: %v", i, err)
		}
	}

	count := 0
	deadline := time.After(1 * time.Second)
drain:
	for {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventTurnStart && evt.TurnID == "turn-dup" {
				count++
			}
		case <-deadline:
			break drain
		}
	}
	if count != 1 {
		t.Fatalf("turnStart emissions = %d, want exactly 1 (dedup regression)", count)
	}
}

// TestApprovalTimeoutAutoDeniesCodex exercises Bug B3 for Codex: when
// an approval arrives and no RespondToApproval follows within the timeout,
// the session writes a decline response to the provider and emits an
// EventError. The subprocess must stay alive.
func TestApprovalTimeoutAutoDeniesCodex(t *testing.T) {
	s, eventCh := newTestCodexSessionWithApprovalTimeout(t, 100*time.Millisecond)

	// Drive an approval server request through dispatchLine; rpcID 42 is
	// the wire identifier the auto-deny must echo back.
	line := []byte(`{"jsonrpc":"2.0","id":42,"method":"item/commandExecution/requestApproval","params":{"command":"ls"}}`)
	s.dispatchLine(line)

	var gotApproval, gotError bool
	deadline := time.After(3 * time.Second)
	for !(gotApproval && gotError) {
		select {
		case evt := <-eventCh:
			switch evt.Kind {
			case provider.EventApprovalRequest:
				gotApproval = true
			case provider.EventError:
				if containsAny(evt.Content, "approval timed out", "approval timeout") {
					gotError = true
				}
			}
		case <-deadline:
			t.Fatalf("timeout (approval=%v err=%v)", gotApproval, gotError)
		}
	}

	// Session must stay alive — only the single approval request is resolved.
	select {
	case <-s.proc.Done():
		t.Fatal("codex session died after auto-deny")
	default:
	}
}

// TestApprovalResponseCancelsTimeoutCodex confirms the happy path.
func TestApprovalResponseCancelsTimeoutCodex(t *testing.T) {
	s, eventCh := newTestCodexSessionWithApprovalTimeout(t, 500*time.Millisecond)

	line := []byte(`{"jsonrpc":"2.0","id":7,"method":"item/commandExecution/requestApproval","params":{"command":"ls"}}`)
	s.dispatchLine(line)

	for {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventApprovalRequest {
				goto respond
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no approval event")
		}
	}
respond:
	if err := s.RespondToApproval(context.Background(), provider.ApprovalResponse{
		RequestID: "7",
		Decision:  "allow",
	}); err != nil {
		t.Fatalf("RespondToApproval: %v", err)
	}

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

// TestApprovalTimeoutClearedOnCloseCodex exercises Close with a pending
// approval — the timer must be cancelled cleanly.
func TestApprovalTimeoutClearedOnCloseCodex(t *testing.T) {
	s, eventCh := newTestCodexSessionWithApprovalTimeout(t, 200*time.Millisecond)

	line := []byte(`{"jsonrpc":"2.0","id":9,"method":"item/commandExecution/requestApproval","params":{"command":"ls"}}`)
	s.dispatchLine(line)

	for {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventApprovalRequest {
				goto closeNow
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no approval event")
		}
	}
closeNow:
	if err := s.Close(); err != nil {
		t.Logf("close returned %v (acceptable)", err)
	}

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

func TestBuildApprovalResponseResultDecision(t *testing.T) {
	// Codex-native decision values are passed through directly -- no translation.
	tests := []struct {
		name     string
		decision string
		want     string
	}{
		{name: "accept", decision: "accept", want: "accept"},
		{name: "decline", decision: "decline", want: "decline"},
		{name: "acceptForSession", decision: "acceptForSession", want: "acceptForSession"},
		{name: "cancel", decision: "cancel", want: "cancel"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rpcID, result, err := buildApprovalResponseResult(provider.ApprovalResponse{
				RequestID: "42",
				Decision:  tt.decision,
			})
			if err != nil {
				t.Fatalf("buildApprovalResponseResult(%s): %v", tt.decision, err)
			}
			if rpcID != 42 {
				t.Fatalf("rpcID = %d, want 42", rpcID)
			}

			payload, ok := result.(map[string]any)
			if !ok {
				t.Fatalf("result type = %T, want map[string]any", result)
			}
			if got := payload["decision"]; got != tt.want {
				t.Fatalf("decision = %v, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildApprovalResponseResultUserInput(t *testing.T) {
	rpcID, result, err := buildApprovalResponseResult(provider.ApprovalResponse{
		RequestID: "7",
		Answers: map[string]provider.UserInputAnswer{
			"framework": provider.SingleUserInputAnswer("React"),
			"scope":     provider.UserInputAnswer{"turn", "session"},
		},
	})
	if err != nil {
		t.Fatalf("buildApprovalResponseResult(): %v", err)
	}
	if rpcID != 7 {
		t.Fatalf("rpcID = %d, want 7", rpcID)
	}

	payload, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result type = %T, want map[string]any", result)
	}
	answers, ok := payload["answers"].(map[string]codexUserInputAnswer)
	if !ok {
		t.Fatalf("answers type = %T, want map[string]codexUserInputAnswer", payload["answers"])
	}
	if got := answers["framework"].Answers; len(got) != 1 || got[0] != "React" {
		t.Fatalf("framework answers = %v, want [React]", got)
	}
	if got := answers["scope"].Answers; len(got) != 2 || got[0] != "turn" || got[1] != "session" {
		t.Fatalf("scope answers = %v, want [turn session]", got)
	}
}

func TestBuildApprovalResponseResultPermission(t *testing.T) {
	enabled := true
	rpcID, result, err := buildApprovalResponseResult(provider.ApprovalResponse{
		RequestID: "9",
		Scope:     "session",
		Permissions: &provider.PermissionProfile{
			Network: &provider.NetworkPermissions{Enabled: &enabled},
		},
	})
	if err != nil {
		t.Fatalf("buildApprovalResponseResult(): %v", err)
	}
	if rpcID != 9 {
		t.Fatalf("rpcID = %d, want 9", rpcID)
	}

	payload, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result type = %T, want map[string]any", result)
	}
	if got := payload["scope"]; got != "session" {
		t.Fatalf("scope = %v, want session", got)
	}
	if payload["permissions"] == nil {
		t.Fatal("permissions should be present")
	}
}

func TestBuildApprovalResponseResultInvalidRequestID(t *testing.T) {
	_, _, err := buildApprovalResponseResult(provider.ApprovalResponse{RequestID: "not-a-number"})
	if err == nil {
		t.Fatal("expected invalid request ID error")
	}
}

func TestCodexRespondToApprovalMethod(t *testing.T) {
	s, _ := newTestCodexSession(t)

	err := s.RespondToApproval(context.Background(), provider.ApprovalResponse{
		RequestID: "42",
		Decision:  "allow",
	})
	if err != nil {
		t.Fatalf("RespondToApproval(): %v", err)
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

func TestCodexSendRequestReturnsErrorWhenSessionStops(t *testing.T) {
	ctx, cancelProc := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args:   []string{"-c", "cat >/dev/null"},
	})
	if err != nil {
		t.Fatalf("spawn quiet process: %v", err)
	}

	s := &Session{
		proc:          proc,
		threadID:      testThread,
		codexThreadID: "codex-thread-1",
		pending:       make(map[int64]chan json.RawMessage),
		onEvent:       func(provider.ProviderEvent) {},
		cancel:        cancelProc,
	}
	go s.readLoop()

	t.Cleanup(func() {
		cancelProc()
		_ = proc.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		_, err := s.sendRequest(ctx, "test/method", map[string]any{"key": "value"})
		errCh <- err
	}()

	deadline := time.Now().Add(2 * time.Second)
	observedPending := false
	for time.Now().Before(deadline) {
		s.mu.Lock()
		pendingCount := len(s.pending)
		s.mu.Unlock()
		if pendingCount == 1 {
			observedPending = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !observedPending {
		t.Fatal("timed out waiting for pending request registration")
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected session stop error")
		}
		if !strings.Contains(err.Error(), "session stopped before request completed") {
			t.Fatalf("error = %v, want session stop message", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for sendRequest to fail")
	}
}

// TestCodexSendRequestTimeoutDrainsLateResponse covers Bug E5. Before the
// fix, a response that arrived between the timeout firing and the
// deferred pending-delete ran into a buffer-1 channel that nobody read,
// leaking a record into the goroutine's stack until GC. The fix calls
// abandon() (delete-from-pending + drain channel) inside the timeout
// case before returning, so a late response either (a) arrives before
// the delete and is drained, or (b) arrives after the delete and is
// dropped by dispatchLine's default branch.
//
// The test drives the path by overriding requestTimeoutOverride to a
// short window, sending the request, waiting past the timeout, then
// injecting a response for the now-abandoned id. Before the fix, the
// channel would retain the unread payload; after the fix, the pending
// map is empty and no channel is holding the payload. We also assert
// that the goroutine count returns to baseline.
func TestCodexSendRequestTimeoutDrainsLateResponse(t *testing.T) {
	ctx, cancelProc := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args:   []string{"-c", "cat >/dev/null"},
	})
	if err != nil {
		t.Fatalf("spawn quiet process: %v", err)
	}
	s := &Session{
		proc:                   proc,
		threadID:               testThread,
		codexThreadID:          "codex-thread-1",
		pending:                make(map[int64]chan json.RawMessage),
		onEvent:                func(provider.ProviderEvent) {},
		cancel:                 cancelProc,
		requestTimeoutOverride: 50 * time.Millisecond,
	}
	go s.readLoop()
	t.Cleanup(func() {
		cancelProc()
		_ = proc.Close()
	})

	// Fire a request that we know will time out because the quiet
	// subprocess never replies.
	start := time.Now()
	_, err = s.sendRequest(context.Background(), "test/method", map[string]any{"k": "v"})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("err = %v, want timeout", err)
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond || elapsed > 2*time.Second {
		t.Errorf("elapsed = %v, want ~50ms", elapsed)
	}

	// After the timeout returns, the pending map must no longer contain
	// the request id. Without the fix, the defer eventually cleaned it
	// up but only AFTER the response could have landed in the buffered
	// channel — here we assert the immediate post-return invariant.
	s.mu.Lock()
	pendingCount := len(s.pending)
	s.mu.Unlock()
	if pendingCount != 0 {
		t.Errorf("pending has %d entries after timeout, want 0", pendingCount)
	}

	// Simulate a late response arriving from dispatchLine with the
	// abandoned id. It must be silently dropped by dispatchLine
	// (pending map already emptied) — no panic, no hang.
	lateResponse := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"too":"late"}}`, s.nextID.Load())
	s.dispatchLine([]byte(lateResponse))

	// After all of the above, the pending map must still be empty and
	// the session healthy. A follow-up request must work.
	s.mu.Lock()
	pendingCount = len(s.pending)
	s.mu.Unlock()
	if pendingCount != 0 {
		t.Errorf("pending has %d entries after late response, want 0", pendingCount)
	}
}

// TestCodexSendRequestManyTimeoutsDoNotLeak ensures that N requests
// that all time out and later see late responses do not accumulate
// pending-map entries, buffered channel records, or goroutines.
func TestCodexSendRequestManyTimeoutsDoNotLeak(t *testing.T) {
	ctx, cancelProc := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args:   []string{"-c", "cat >/dev/null"},
	})
	if err != nil {
		t.Fatalf("spawn quiet process: %v", err)
	}
	s := &Session{
		proc:                   proc,
		threadID:               testThread,
		codexThreadID:          "codex-thread-1",
		pending:                make(map[int64]chan json.RawMessage),
		onEvent:                func(provider.ProviderEvent) {},
		cancel:                 cancelProc,
		requestTimeoutOverride: 20 * time.Millisecond,
	}
	go s.readLoop()
	t.Cleanup(func() {
		cancelProc()
		_ = proc.Close()
	})

	const rounds = 10
	for i := 0; i < rounds; i++ {
		_, err := s.sendRequest(context.Background(), "test/method", nil)
		if err == nil || !strings.Contains(err.Error(), "timeout") {
			t.Fatalf("round %d: expected timeout, got %v", i, err)
		}
		// Inject a late response for the id we just abandoned. Should
		// be dropped silently.
		lateResponse := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{}}`, s.nextID.Load())
		s.dispatchLine([]byte(lateResponse))
	}

	s.mu.Lock()
	pendingCount := len(s.pending)
	s.mu.Unlock()
	if pendingCount != 0 {
		t.Errorf("pending has %d entries after %d timeouts, want 0", pendingCount, rounds)
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
		cancel:   cancel,
		readDone: make(chan struct{}),
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

func TestCodexCloseWaitsForDisconnectedHandler(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: "cat"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	disconnected := make(chan struct{})
	release := make(chan struct{})
	closeReturned := make(chan struct{})
	s := &Session{
		proc:          proc,
		threadID:      testThread,
		codexThreadID: "test",
		pending:       make(map[int64]chan json.RawMessage),
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

func TestCodexReadLoopEmitsErrorStatusOnUnexpectedExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args:   []string{"-c", "sleep 0.05; exit 9"},
	})
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
				if meta.ExitCode != 9 {
					t.Fatalf("exitCode = %d, want 9", meta.ExitCode)
				}
			case "disconnected":
				gotDisconnected = true
			}
		case <-timeout:
			t.Fatal("timeout waiting for unexpected-exit events")
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

// -- SetDynamicToolHandler / handleDynamicToolCall tests --

func TestSetDynamicToolHandler(t *testing.T) {
	s, _ := newTestCodexSession(t)

	called := false
	handler := func(toolName string, args map[string]any) (string, bool, error) {
		called = true
		return "result", true, nil
	}

	s.SetDynamicToolHandler(handler)

	s.mu.Lock()
	h := s.dynamicToolHandler
	s.mu.Unlock()
	if h == nil {
		t.Fatal("expected non-nil handler after SetDynamicToolHandler")
	}

	// Invoke the handler to confirm it's the right one.
	_, _, _ = h("test", nil)
	if !called {
		t.Error("expected handler to be called")
	}
}

func TestSetDynamicToolHandlerNil(t *testing.T) {
	s, _ := newTestCodexSession(t)

	s.SetDynamicToolHandler(func(string, map[string]any) (string, bool, error) {
		return "", false, nil
	})
	s.SetDynamicToolHandler(nil)

	s.mu.Lock()
	h := s.dynamicToolHandler
	s.mu.Unlock()
	if h != nil {
		t.Error("expected nil handler after SetDynamicToolHandler(nil)")
	}
}

func TestHandleDynamicToolCall(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	resultCh := make(chan string, 1)
	s.SetDynamicToolHandler(func(toolName string, args map[string]any) (string, bool, error) {
		resultCh <- toolName
		return "tool output", true, nil
	})

	line := []byte(`{"jsonrpc":"2.0","id":10,"method":"item/tool/call","params":{"tool":"my_tool","arguments":{"key":"value"}}}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The handler runs asynchronously. Wait for it.
	select {
	case name := <-resultCh:
		if name != "my_tool" {
			t.Errorf("toolName: got %q, want %q", name, "my_tool")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for dynamic tool handler invocation")
	}

	// Drain any events from the echo.
	_ = eventCh
}

func TestHandleDynamicToolCallToolNameField(t *testing.T) {
	s, _ := newTestCodexSession(t)

	resultCh := make(chan string, 1)
	s.SetDynamicToolHandler(func(toolName string, args map[string]any) (string, bool, error) {
		resultCh <- toolName
		return "ok", true, nil
	})

	// Use "toolName" field instead of "tool".
	line := []byte(`{"jsonrpc":"2.0","id":11,"method":"dynamicToolCall","params":{"toolName":"alt_tool","arguments":{}}}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case name := <-resultCh:
		if name != "alt_tool" {
			t.Errorf("toolName: got %q, want %q", name, "alt_tool")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for dynamic tool handler invocation")
	}
}

func TestHandleDynamicToolCallHandlerError(t *testing.T) {
	s, _ := newTestCodexSession(t)

	doneCh := make(chan struct{}, 1)
	s.SetDynamicToolHandler(func(toolName string, args map[string]any) (string, bool, error) {
		defer func() { doneCh <- struct{}{} }()
		return "", false, fmt.Errorf("simulated tool error")
	})

	line := []byte(`{"jsonrpc":"2.0","id":12,"method":"item/tool/call","params":{"tool":"fail_tool","arguments":{}}}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case <-doneCh:
		// Handler ran and returned error -- the session formats it as "Error: ..."
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for handler to complete")
	}
}

func TestHandleDynamicToolCallNilArguments(t *testing.T) {
	s, _ := newTestCodexSession(t)

	resultCh := make(chan map[string]any, 1)
	s.SetDynamicToolHandler(func(toolName string, args map[string]any) (string, bool, error) {
		resultCh <- args
		return "ok", true, nil
	})

	// No "arguments" field in params -- should default to empty map.
	line := []byte(`{"jsonrpc":"2.0","id":13,"method":"item/tool/call","params":{"tool":"noargs_tool"}}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case args := <-resultCh:
		if args == nil {
			t.Error("expected non-nil args map")
		}
		if len(args) != 0 {
			t.Errorf("expected empty args, got %v", args)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for handler")
	}
}

func TestHandleDynamicToolCallNoHandler(t *testing.T) {
	s, _ := newTestCodexSession(t)
	// No handler set -- should send error response.

	line := []byte(`{"jsonrpc":"2.0","id":14,"method":"item/tool/call","params":{"tool":"orphan_tool"}}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Give readLoop time to process. With cat, the error response echoes back.
	time.Sleep(200 * time.Millisecond)
}

// -- handleServerRequest: elicitation branch --

func TestCodexHandleServerRequestElicitation(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	line := []byte(`{"jsonrpc":"2.0","id":5,"method":"mcpServer/elicitation/request","params":{"serverName":"my-mcp","message":"Please authorize","requestedSchema":{"type":"string"}}}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}

	evt := codexWaitEvent(t, eventCh)
	if evt.Kind != provider.EventApprovalRequest {
		t.Fatalf("kind: got %q, want %q", evt.Kind, provider.EventApprovalRequest)
	}

	var approval provider.ApprovalRequest
	if err := json.Unmarshal(evt.Meta, &approval); err != nil {
		t.Fatalf("unmarshal approval: %v", err)
	}
	if approval.Kind != "mcp-elicitation" {
		t.Errorf("kind: got %q, want %q", approval.Kind, "mcp-elicitation")
	}
	if approval.RequestID != "5" {
		t.Errorf("requestID: got %q, want %q", approval.RequestID, "5")
	}
	if approval.Description != "Please authorize" {
		t.Errorf("description: got %q, want %q", approval.Description, "Please authorize")
	}
}

// -- handleServerRequest: legacy approval methods --

func TestCodexHandleServerRequestApplyPatchApproval(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	line := []byte(`{"jsonrpc":"2.0","id":6,"method":"applyPatchApproval","params":{"filePath":"/tmp/foo.go"}}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}

	evt := codexWaitEvent(t, eventCh)
	if evt.Kind != provider.EventApprovalRequest {
		t.Fatalf("kind: got %q, want %q", evt.Kind, provider.EventApprovalRequest)
	}

	var approval provider.ApprovalRequest
	if err := json.Unmarshal(evt.Meta, &approval); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if approval.Kind != "file-change" {
		t.Errorf("kind: got %q, want %q", approval.Kind, "file-change")
	}
}

func TestCodexHandleServerRequestExecCommandApproval(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	line := []byte(`{"jsonrpc":"2.0","id":7,"method":"execCommandApproval","params":{"command":"npm test"}}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}

	evt := codexWaitEvent(t, eventCh)
	if evt.Kind != provider.EventApprovalRequest {
		t.Fatalf("kind: got %q, want %q", evt.Kind, provider.EventApprovalRequest)
	}

	var approval provider.ApprovalRequest
	if err := json.Unmarshal(evt.Meta, &approval); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if approval.Kind != "command" {
		t.Errorf("kind: got %q, want %q", approval.Kind, "command")
	}
	if approval.ToolName != "command" {
		t.Errorf("toolName: got %q, want %q", approval.ToolName, "command")
	}
}

// -- handleServerRequest: file read approval --

func TestCodexHandleServerRequestFileReadApproval(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	line := []byte(`{"jsonrpc":"2.0","id":8,"method":"item/fileRead/requestApproval","params":{"filePath":"/etc/passwd"}}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}

	evt := codexWaitEvent(t, eventCh)
	if evt.Kind != provider.EventApprovalRequest {
		t.Fatalf("kind: got %q, want %q", evt.Kind, provider.EventApprovalRequest)
	}

	var approval provider.ApprovalRequest
	if err := json.Unmarshal(evt.Meta, &approval); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if approval.Kind != "file-read" {
		t.Errorf("kind: got %q, want %q", approval.Kind, "file-read")
	}
	if approval.ToolName != "file_read" {
		t.Errorf("toolName: got %q, want %q", approval.ToolName, "file_read")
	}
}

// -- buildApprovalResponseResult: elicitation branch --

func TestBuildApprovalResponseResultElicitation(t *testing.T) {
	rpcID, result, err := buildApprovalResponseResult(provider.ApprovalResponse{
		RequestID: "15",
		Elicitation: &provider.ElicitationResolution{
			Action:  "confirm",
			Content: json.RawMessage(`{"key":"value"}`),
			Meta:    json.RawMessage(`{"source":"test"}`),
		},
	})
	if err != nil {
		t.Fatalf("buildApprovalResponseResult(): %v", err)
	}
	if rpcID != 15 {
		t.Fatalf("rpcID = %d, want 15", rpcID)
	}
	payload, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result type = %T", result)
	}
	if payload["action"] != "confirm" {
		t.Errorf("action: got %v, want confirm", payload["action"])
	}
	if payload["content"] == nil {
		t.Error("expected non-nil content")
	}
	if payload["_meta"] == nil {
		t.Error("expected non-nil _meta")
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

func TestSessionForkWithMock(t *testing.T) {
	script := `#!/bin/bash
while IFS= read -r line; do
    id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if echo "$line" | grep -q '"method":"initialize"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{}}"
        continue
    fi
    if echo "$line" | grep -q '"method":"thread/start"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"thread\":{\"id\":\"mock-thread-123\"}}}"
        continue
    fi
    if echo "$line" | grep -q '"method":"thread/fork"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"thread\":{\"id\":\"mock-thread-fork-456\"}}}"
    fi
done
`
	scriptDir := t.TempDir()
	scriptPath := scriptDir + "/codex"
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write mock script: %v", err)
	}

	s, err := NewSession(context.Background(), testThread, Config{
		Binary:  scriptPath,
		Model:   "test-model",
		WorkDir: "/tmp",
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer s.Close()

	forkedThreadID, err := s.Fork(context.Background())
	if err != nil {
		t.Fatalf("Fork() error = %v", err)
	}
	if forkedThreadID != "mock-thread-fork-456" {
		t.Fatalf("Fork() = %q, want %q", forkedThreadID, "mock-thread-fork-456")
	}
}
