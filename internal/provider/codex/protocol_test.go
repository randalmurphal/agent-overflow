package codex

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

// -- ClassifyNotification dedicated tests --

func TestClassifyNotification_TurnStarted(t *testing.T) {
	tests := []struct {
		name   string
		params string
		turnID string
	}{
		{
			name:   "normal",
			params: `{"turn":{"id":"turn-1"}}`,
			turnID: "turn-1",
		},
		{
			name:   "missing turn id",
			params: `{"turn":{}}`,
			turnID: "",
		},
		{
			name:   "empty params",
			params: `{}`,
			turnID: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events := ClassifyNotification("t1", "turn/started", json.RawMessage(tt.params))
			if len(events) != 1 {
				t.Fatalf("expected 1 event, got %d", len(events))
			}
			if events[0].Kind != provider.EventTurnStart {
				t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventTurnStart)
			}
			if events[0].TurnID != tt.turnID {
				t.Errorf("turnID: got %q, want %q", events[0].TurnID, tt.turnID)
			}
		})
	}
}

func TestClassifyNotification_TurnCompleted_WithUsage(t *testing.T) {
	params := `{"turn":{"id":"t1","status":"completed","usage":{"inputTokens":100,"outputTokens":50},"model":"claude-sonnet-4-6"},"model":"claude-sonnet-4-6"}`
	events := ClassifyNotification("thread-1", "turn/completed", json.RawMessage(params))

	// Should emit usage event + turn complete.
	hasUsage := false
	hasComplete := false
	for _, evt := range events {
		if evt.Kind == provider.EventTokenUsage {
			hasUsage = true
		}
		if evt.Kind == provider.EventTurnComplete {
			hasComplete = true
		}
	}

	if !hasUsage {
		t.Error("expected a token usage event")
	}
	if !hasComplete {
		t.Error("expected a turn complete event")
	}
}

func TestClassifyNotification_TurnAborted(t *testing.T) {
	params := `{"turn":{"id":"turn-5"}}`
	events := ClassifyNotification("t1", "turn/aborted", json.RawMessage(params))

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	evt := events[0]
	if evt.Kind != provider.EventTurnComplete {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventTurnComplete)
	}
	if evt.TurnID != "turn-5" {
		t.Errorf("turnID: got %q, want %q", evt.TurnID, "turn-5")
	}

	// Meta should contain aborted: true.
	var meta map[string]bool
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if !meta["aborted"] {
		t.Error("expected meta.aborted to be true")
	}
}

func TestClassifyNotification_ItemUpdated(t *testing.T) {
	params := `{"item":{"id":"item-9","type":"command_execution","status":"in_progress"}}`
	events := ClassifyNotification("t1", "item/updated", json.RawMessage(params))

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	evt := events[0]
	if evt.Kind != provider.EventToolStart {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventToolStart)
	}
	if evt.ItemID != "item-9" {
		t.Errorf("itemID: got %q, want %q", evt.ItemID, "item-9")
	}
	if evt.ItemType != "command_execution" {
		t.Errorf("itemType: got %q, want %q", evt.ItemType, "command_execution")
	}
	if !evt.Replace {
		t.Error("expected Replace=true for item/updated")
	}
}

func TestClassifyNotification_ErrorWithWillRetry(t *testing.T) {
	params := `{"error":{"message":"Reconnecting... 2/5"},"willRetry":true}`
	events := ClassifyNotification("t1", "error", json.RawMessage(params))

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
}

func TestClassifyNotification_ErrorWithoutWillRetry(t *testing.T) {
	params := `{"error":{"message":"fatal error"}}`
	events := ClassifyNotification("t1", "error", json.RawMessage(params))

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	evt := events[0]
	if evt.Kind != provider.EventError {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventError)
	}
	if evt.Content != "fatal error" {
		t.Errorf("content: got %q, want %q", evt.Content, "fatal error")
	}
}

func TestClassifyNotification_ErrorWillRetryFalse(t *testing.T) {
	params := `{"error":{"message":"giving up"},"willRetry":false}`
	events := ClassifyNotification("t1", "error", json.RawMessage(params))

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventError {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventError)
	}
}

func TestClassifyNotification_UnknownMethod(t *testing.T) {
	events := ClassifyNotification("t1", "some/future/method", json.RawMessage(`{}`))
	if len(events) != 0 {
		t.Errorf("expected 0 events, got %d", len(events))
	}
}

func TestClassifyNotification_MalformedParams(t *testing.T) {
	events := ClassifyNotification("t1", "turn/started", json.RawMessage(`not json`))
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	// Should still emit an event with empty turn ID.
	if events[0].TurnID != "" {
		t.Errorf("expected empty turnID for malformed params, got %q", events[0].TurnID)
	}
}

func TestClassifyNotification_ItemAgentMessageDeltaEmpty(t *testing.T) {
	events := ClassifyNotification("t1", "item/agentMessage/delta", json.RawMessage(`{"delta":""}`))
	if len(events) != 0 {
		t.Errorf("expected 0 events for empty delta, got %d", len(events))
	}
}

// -- readTopLevelBool tests --

func TestReadTopLevelBool(t *testing.T) {
	tests := []struct {
		name string
		data string
		key  string
		want bool
	}{
		{"true value", `{"willRetry":true}`, "willRetry", true},
		{"false value", `{"willRetry":false}`, "willRetry", false},
		{"missing key", `{"other":true}`, "willRetry", false},
		{"non-bool value", `{"willRetry":"yes"}`, "willRetry", false},
		{"invalid json", `not json`, "willRetry", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := readTopLevelBool(json.RawMessage(tt.data), tt.key)
			if got != tt.want {
				t.Errorf("readTopLevelBool(%s, %q) = %v, want %v", tt.data, tt.key, got, tt.want)
			}
		})
	}
}

// -- extractUsageFromTurn tests --

func TestExtractUsageFromTurn_TopLevel(t *testing.T) {
	params := json.RawMessage(`{"usage":{"inputTokens":100,"outputTokens":50}}`)
	result := extractUsageFromTurn(params)
	if result == nil {
		t.Fatal("expected non-nil usage data")
	}
}

func TestExtractUsageFromTurn_NestedTurn(t *testing.T) {
	params := json.RawMessage(`{"turn":{"usage":{"inputTokens":200,"outputTokens":100}}}`)
	result := extractUsageFromTurn(params)
	if result == nil {
		t.Fatal("expected non-nil usage data")
	}
}

func TestExtractUsageFromTurn_NoUsage(t *testing.T) {
	params := json.RawMessage(`{"turn":{"id":"t1","status":"completed"}}`)
	result := extractUsageFromTurn(params)
	if result != nil {
		t.Errorf("expected nil for missing usage, got %s", string(result))
	}
}

func TestExtractUsageFromTurn_WithModelComputesCost(t *testing.T) {
	params := json.RawMessage(`{"usage":{"inputTokens":1000000,"outputTokens":500000},"model":"claude-sonnet-4-6"}`)
	result := extractUsageFromTurn(params)
	if result == nil {
		t.Fatal("expected non-nil result")
	}

	var usage provider.TokenUsage
	if err := json.Unmarshal(result, &usage); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// claude-sonnet: $3.00/M input + $15.00/M output = $3.00 + $7.50 = $10.50
	if usage.TotalCostUSD < 10.0 || usage.TotalCostUSD > 11.0 {
		t.Errorf("TotalCostUSD: got %f, want ~10.50", usage.TotalCostUSD)
	}
}

func TestExtractUsageFromTurn_InvalidJSON(t *testing.T) {
	result := extractUsageFromTurn(json.RawMessage(`not json`))
	if result != nil {
		t.Error("expected nil for invalid JSON")
	}
}
