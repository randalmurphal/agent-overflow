package codex

import (
	"encoding/json"
	"strings"
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

func TestClassifyNotification_TurnCompleted_DoesNotEmitUsage(t *testing.T) {
	params := `{"threadId":"thread-1","turn":{"id":"t1","items":[],"status":"completed","error":null,"startedAt":1777926299,"completedAt":1777926306,"durationMs":6637}}`
	events := ClassifyNotification("thread-1", "turn/completed", json.RawMessage(params))

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

	if hasUsage {
		t.Error("turn/completed must not emit token usage; thread/tokenUsage/updated is the context signal")
	}
	if !hasComplete {
		t.Error("expected a turn complete event")
	}
}

func TestClassifyNotification_ThreadTokenUsageUpdatedNormalizesContextWindow(t *testing.T) {
	params := json.RawMessage(`{"tokenUsage":{"last":{"inputTokens":100,"outputTokens":20,"cachedInputTokens":6,"totalTokens":126},"total":{"inputTokens":9000,"outputTokens":2000,"cachedInputTokens":839,"totalTokens":11839},"modelContextWindow":258400}}`)
	events := ClassifyNotification("thread-1", "thread/tokenUsage/updated", params)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventTokenUsage {
		t.Fatalf("kind: got %q, want %q", events[0].Kind, provider.EventTokenUsage)
	}

	var window provider.ContextWindow
	if err := json.Unmarshal(events[0].Meta, &window); err != nil {
		t.Fatalf("unmarshal context window: %v", err)
	}
	if window.UsedTokens != 126 {
		t.Fatalf("usedTokens: got %d, want 126", window.UsedTokens)
	}
	if window.MaxTokens != 258400 {
		t.Fatalf("maxTokens: got %d, want 258400", window.MaxTokens)
	}
}

func TestClassifyNotification_ThreadTokenUsageUpdatedDoesNotSumBreakdowns(t *testing.T) {
	params := json.RawMessage(`{"tokenUsage":{"last":{"inputTokens":100,"outputTokens":20,"cachedInputTokens":6,"reasoningOutputTokens":4},"total":{"inputTokens":9000,"outputTokens":2000,"cachedInputTokens":839},"modelContextWindow":258400}}`)
	events := ClassifyNotification("thread-1", "thread/tokenUsage/updated", params)

	if len(events) != 1 {
		t.Fatalf("expected 1 event because max window is still useful, got %d", len(events))
	}
	var window provider.ContextWindow
	if err := json.Unmarshal(events[0].Meta, &window); err != nil {
		t.Fatalf("unmarshal context window: %v", err)
	}
	if window.UsedTokens != 0 {
		t.Fatalf("usedTokens: got %d, want 0; Codex totalTokens is the context signal", window.UsedTokens)
	}
	if window.MaxTokens != 258400 {
		t.Fatalf("maxTokens: got %d, want 258400", window.MaxTokens)
	}
}

// TestClassifyNotification_ThreadTokenUsageUpdatedDetectsExceededSentinel
// pins the Codex `ContextWindowExceeded` sentinel:
// `fill_to_context_window` (codex-rs/protocol/src/protocol.rs:2040) sets
// `total.totalTokens = modelContextWindow` exactly while
// `last.totalTokens` becomes `(window - previous_total).max(0)` — a small
// delta in the realistic case (previous_total > 0), NOT the window. So
// checking `last == window` would only fire on the very first turn; the
// load-bearing signal is `total.totalTokens == modelContextWindow`. The
// parser surfaces it as `ContextWindow.Exceeded` so the meter renders a
// distinct state.
func TestClassifyNotification_ThreadTokenUsageUpdatedDetectsExceededSentinel(t *testing.T) {
	// Realistic shape: previous_total=250000, window=258400 → delta=8400 in `last`,
	// `total.totalTokens` pegged to 258400 (the sentinel).
	params := json.RawMessage(`{"tokenUsage":{"last":{"totalTokens":8400},"total":{"totalTokens":258400},"modelContextWindow":258400}}`)
	events := ClassifyNotification("thread-1", "thread/tokenUsage/updated", params)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	var window provider.ContextWindow
	if err := json.Unmarshal(events[0].Meta, &window); err != nil {
		t.Fatalf("unmarshal context window: %v", err)
	}
	if !window.Exceeded {
		t.Fatalf("expected Exceeded=true for sentinel total==modelContextWindow, got %+v", window)
	}
	if window.MaxTokens != 258400 {
		t.Fatalf("expected max equal to 258400, got %+v", window)
	}
}

// TestClassifyNotification_ThreadTokenUsageUpdatedDoesNotFalseFireExceeded
// guards against false-firing the sentinel for ordinary readings —
// including the case where `last.totalTokens == modelContextWindow` but
// `total.totalTokens` is the rolling aggregate (NOT the sentinel). The
// rolling aggregate is what the wire docs describe as "total processed
// across messages" and must NOT trigger the exceeded state on its own.
func TestClassifyNotification_ThreadTokenUsageUpdatedDoesNotFalseFireExceeded(t *testing.T) {
	// Ordinary high reading: total < window, so no sentinel.
	params := json.RawMessage(`{"tokenUsage":{"last":{"totalTokens":258399},"total":{"totalTokens":11839},"modelContextWindow":258400}}`)
	events := ClassifyNotification("thread-1", "thread/tokenUsage/updated", params)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	var window provider.ContextWindow
	if err := json.Unmarshal(events[0].Meta, &window); err != nil {
		t.Fatalf("unmarshal context window: %v", err)
	}
	if window.Exceeded {
		t.Fatalf("expected Exceeded=false for normal high reading (last==window but total!=window), got %+v", window)
	}
}

// TestClassifyNotification_ItemUpdatedIsPhantom pins that the Codex
// app-server wire protocol has NO `item/updated` method — it only
// emits `item/started` and `item/completed`. Reference:
// /home/rmurphy/repos/codex/codex-rs/app-server-protocol/schema/typescript/ServerNotification.ts.
// Any classifier branch that produces events for `item/updated` would
// be dispatching on a phantom method; this test locks that in by
// asserting zero events come out for such a method.
func TestClassifyNotification_ItemUpdatedIsPhantom(t *testing.T) {
	params := `{"item":{"id":"item-9","type":"command_execution","status":"in_progress"}}`
	events := ClassifyNotification("t1", "item/updated", json.RawMessage(params))
	if len(events) != 0 {
		t.Errorf("expected no events for phantom method item/updated, got %d: %+v", len(events), events)
	}
}

func TestClassifyNotification_ItemCompletedPlan(t *testing.T) {
	params := `{"item":{"id":"plan-1","type":"plan","text":"# Ship it\n\n- one\n- two"}}`
	events := ClassifyNotification("t1", "item/completed", json.RawMessage(params))

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	evt := events[0]
	if evt.Kind != provider.EventProposedPlan {
		t.Fatalf("kind: got %q, want %q", evt.Kind, provider.EventProposedPlan)
	}
	if evt.ItemID != "plan-1" {
		t.Fatalf("itemID: got %q, want %q", evt.ItemID, "plan-1")
	}
	if evt.Content != "# Ship it\n\n- one\n- two" {
		t.Fatalf("content: got %q, want %q", evt.Content, "# Ship it\n\n- one\n- two")
	}
}

// TestClassifyItemCompleted_UserMessage_EmitsEventUserText covers the
// canonical Codex `userMessage` shape: `item/completed` with
// `item.content` as a `[{type:"text",text:"..."}]` array. The
// classifier promotes this to a single EventUserText whose meta
// carries `provider_item_id` set to `item.id`. Phase E reads that
// key to stamp the AO-owned `user:<turnIndex>` row.
func TestClassifyItemCompleted_UserMessage_EmitsEventUserText(t *testing.T) {
	params := `{"turnId":"turn-9","item":{"id":"item-abc","type":"userMessage","content":[{"type":"text","text":"hi from user"}]}}`
	events := ClassifyNotification("thread-1", "item/completed", json.RawMessage(params))

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}
	evt := events[0]
	if evt.Kind != provider.EventUserText {
		t.Fatalf("kind: got %q, want %q", evt.Kind, provider.EventUserText)
	}
	if evt.Content != "hi from user" {
		t.Fatalf("content: got %q, want %q", evt.Content, "hi from user")
	}
	if evt.TurnID != "turn-9" {
		t.Fatalf("turnID: got %q, want %q", evt.TurnID, "turn-9")
	}
	if evt.ItemID != "" {
		t.Fatalf("itemID: got %q, want empty (triage owns the AO row id)", evt.ItemID)
	}

	var meta map[string]any
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["provider_item_id"] != "item-abc" {
		t.Fatalf("meta.provider_item_id: got %v, want item-abc", meta["provider_item_id"])
	}
}

func TestClassifyItemCompleted_UserMessage_SubagentNotificationStillEmitsUserText(t *testing.T) {
	params := `{"turnId":"turn-9","item":{"id":"item-subagent-note","type":"userMessage","content":[{"type":"text","text":"<subagent_notification>{\"agent_path\":\"child-1\",\"status\":\"completed\"}</subagent_notification>"}]}}`
	events := ClassifyNotification("thread-1", "item/completed", json.RawMessage(params))

	if len(events) != 1 {
		t.Fatalf("expected 1 user text event before session-level carrier suppression, got %d: %+v", len(events), events)
	}
	if events[0].Kind != provider.EventUserText {
		t.Fatalf("event kind = %v, want EventUserText", events[0].Kind)
	}
}

// TestClassifyItemCompleted_UserMessage_StringContent covers the
// defensive secondary shape — `content` as a plain string. Real
// Codex captures haven't shown this for userMessage, but the wire
// schema's content union allows it (matches Claude's SDK replay
// shape) so we accept it rather than drop on the assumption that
// only the array form lands.
func TestClassifyItemCompleted_UserMessage_StringContent(t *testing.T) {
	params := `{"turnId":"turn-9","item":{"id":"item-str","type":"userMessage","content":"hi"}}`
	events := ClassifyNotification("thread-1", "item/completed", json.RawMessage(params))

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}
	if events[0].Kind != provider.EventUserText {
		t.Fatalf("kind: got %q, want %q", events[0].Kind, provider.EventUserText)
	}
	if events[0].Content != "hi" {
		t.Fatalf("content: got %q, want %q", events[0].Content, "hi")
	}
}

// TestClassifyItemCompleted_UserMessage_MissingItemID covers the
// missing-uuid case — the classifier must still emit EventUserText
// (the wire echo carries semantic value) but must NOT emit an
// empty-string `provider_item_id`. Phase E treats absence-of-key
// and empty-string differently; we collapse them here so triage
// sees one shape.
func TestClassifyItemCompleted_UserMessage_MissingItemID(t *testing.T) {
	params := `{"turnId":"turn-9","item":{"type":"userMessage","content":[{"type":"text","text":"no uuid"}]}}`
	events := ClassifyNotification("thread-1", "item/completed", json.RawMessage(params))

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}
	if events[0].Kind != provider.EventUserText {
		t.Fatalf("kind: got %q, want %q", events[0].Kind, provider.EventUserText)
	}
	if events[0].Content != "no uuid" {
		t.Fatalf("content: got %q, want %q", events[0].Content, "no uuid")
	}

	// Either absent meta or meta without provider_item_id is acceptable;
	// what is NOT acceptable is meta carrying an empty-string value for
	// the key.
	if len(events[0].Meta) == 0 {
		return
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if v, ok := meta["provider_item_id"]; ok {
		if s, isStr := v.(string); !isStr || s == "" {
			t.Fatalf("meta.provider_item_id present but empty/invalid: %v", v)
		}
	}
}

// TestClassifyItemCompleted_UserMessage_MultiTextBlocks covers the
// concatenation rule: every text block's `text` field is appended in
// order with no separator. Image and other non-text blocks are
// silently skipped — mirrors extractToolResultText in
// claude/parse_user.go so the receive-side promotion behaves the
// same on both providers.
func TestClassifyItemCompleted_UserMessage_MultiTextBlocks(t *testing.T) {
	params := `{"turnId":"turn-9","item":{"id":"item-multi","type":"userMessage","content":[{"type":"text","text":"hello "},{"type":"image","source":"data:..."},{"type":"text","text":"world"}]}}`
	events := ClassifyNotification("thread-1", "item/completed", json.RawMessage(params))

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}
	if events[0].Kind != provider.EventUserText {
		t.Fatalf("kind: got %q, want %q", events[0].Kind, provider.EventUserText)
	}
	if events[0].Content != "hello world" {
		t.Fatalf("content: got %q, want %q", events[0].Content, "hello world")
	}
}

// TestClassifyItemNotification_UserMessageStarted_StillDropped pins
// today's behavior: `item/started` for `userMessage` returns no
// events. The started event is the in-flight half of the user
// envelope and has no UI signal we want — only the completed half
// promotes to EventUserText.
func TestClassifyItemNotification_UserMessageStarted_StillDropped(t *testing.T) {
	params := `{"turnId":"turn-9","item":{"id":"item-abc","type":"userMessage","content":[{"type":"text","text":"hi from user"}]}}`
	events := ClassifyNotification("thread-1", "item/started", json.RawMessage(params))
	if len(events) != 0 {
		t.Fatalf("expected 0 events for item/started userMessage, got %d: %+v", len(events), events)
	}
}

// TestClassifyItemCompleted_AgentMessageSettlesText pins the Codex
// multi-message turn boundary. Deltas create the assistant_text row;
// item/completed closes that streaming row so a later tool or final_answer
// message in the same turn gets its own timeline slot.
func TestClassifyItemCompleted_AgentMessageSettlesText(t *testing.T) {
	params := `{"turnId":"turn-9","item":{"id":"agent-1","type":"agentMessage","text":"final answer"}}`
	events := ClassifyNotification("thread-1", "item/completed", json.RawMessage(params))
	if len(events) != 1 {
		t.Fatalf("expected 1 event for item/completed agentMessage, got %d: %+v", len(events), events)
	}
	evt := events[0]
	if evt.Kind != provider.EventContentBlockStop {
		t.Fatalf("kind = %q, want content block stop", evt.Kind)
	}
	if evt.TurnID != "turn-9" {
		t.Fatalf("turnID = %q, want turn-9", evt.TurnID)
	}
	if evt.ItemID != "agent-1" {
		t.Fatalf("itemID = %q, want agent-1", evt.ItemID)
	}
	if evt.Content != "final answer" {
		t.Fatalf("content = %q, want final answer", evt.Content)
	}
	if !evt.ContentPresent {
		t.Fatal("ContentPresent = false, want true")
	}
	var meta map[string]string
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if meta["blockType"] != "text" {
		t.Fatalf("blockType = %q, want text", meta["blockType"])
	}
}

func TestClassifyNotification_ErrorWithWillRetry(t *testing.T) {
	params := `{"error":{"message":"Reconnecting... 2/5"},"willRetry":true}`
	events := ClassifyNotification("t1", "error", json.RawMessage(params))

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	evt := events[0]
	if evt.Kind != provider.EventAPIRetry {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventAPIRetry)
	}
	var meta map[string]any
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	// The "2/5" embedded in the message text drives attempt/max_retries.
	if got, want := int(meta["attempt"].(float64)), 2; got != want {
		t.Errorf("meta.attempt: got %d, want %d", got, want)
	}
	if got, want := int(meta["max_retries"].(float64)), 5; got != want {
		t.Errorf("meta.max_retries: got %d, want %d", got, want)
	}
	if got, want := meta["error"], "Reconnecting... 2/5"; got != want {
		t.Errorf("meta.error: got %q, want %q", got, want)
	}
}

func TestParseCodexRetryCounts(t *testing.T) {
	cases := []struct {
		name        string
		message     string
		wantAttempt int
		wantMax     int
	}{
		{"plain pair", "Reconnecting... 2/5", 2, 5},
		{"no pair", "Reconnecting...", 0, 0},
		{"empty string", "", 0, 0},
		{"multi-digit", "attempt 123/500", 123, 500},
		{"adjacent digits don't match", "id1234567/890done", 0, 0},
		{"finds first valid after stray digits", "code 999999/0 then 2/5", 2, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, m := parseCodexRetryCounts(tc.message)
			if a != tc.wantAttempt || m != tc.wantMax {
				t.Errorf("got (%d, %d), want (%d, %d)", a, m, tc.wantAttempt, tc.wantMax)
			}
		})
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
	// Absent willRetry treated identically to willRetry:false — fatal.
	var meta map[string]any
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if fatal, _ := meta["fatal"].(bool); !fatal {
		t.Errorf("meta.fatal: got %v, want true", meta["fatal"])
	}
}

func TestClassifyNotification_ErrorWillRetryFalse(t *testing.T) {
	params := `{"error":{"message":"giving up"},"willRetry":false}`
	events := ClassifyNotification("t1", "error", json.RawMessage(params))

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	evt := events[0]
	if evt.Kind != provider.EventError {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventError)
	}
	// `willRetry:false` is fatal — meta.fatal:true so the triage router's
	// fatal branch closes the open turn instead of treating the error as
	// recoverable.
	var meta map[string]any
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if fatal, _ := meta["fatal"].(bool); !fatal {
		t.Errorf("meta.fatal: got %v, want true", meta["fatal"])
	}
	if got, want := meta["error"], "giving up"; got != want {
		t.Errorf("meta.error: got %q, want %q", got, want)
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

func TestClassifyNotification_CollabSpawnUsesCollabAgentType(t *testing.T) {
	params := json.RawMessage(`{"item":{"id":"call-1","type":"collabAgentToolCall","tool":"spawnAgent","prompt":"Refactor auth","receiverThreadIds":["child-1"],"newAgentNickname":"Galileo","newAgentRole":"explorer","status":"completed"}}`)
	events := ClassifyNotification("t1", "item/completed", params)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].ItemType != "collab_agent" {
		t.Fatalf("itemType: got %q, want %q", events[0].ItemType, "collab_agent")
	}

	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["toolName"] != "collab_agent" {
		t.Fatalf("toolName: got %v, want collab_agent", meta["toolName"])
	}
	input, ok := meta["input"].(map[string]any)
	if !ok {
		t.Fatalf("input missing from meta: %+v", meta)
	}
	if input["prompt"] != "Refactor auth" {
		t.Fatalf("prompt: got %v, want %q", input["prompt"], "Refactor auth")
	}
	if input["newAgentNickname"] != "Galileo" || input["newAgentRole"] != "explorer" {
		t.Fatalf("agent metadata not surfaced: %+v", input)
	}
}

func TestClassifyNotification_FailedCollabSpawnCarriesFailedStatus(t *testing.T) {
	params := json.RawMessage(`{"item":{"id":"call-1","type":"collabAgentToolCall","tool":"spawnAgent","prompt":"Spawn beyond the limit","receiverThreadIds":[],"status":"failed"}}`)
	events := ClassifyNotification("t1", "item/completed", params)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].ItemType != "collab_agent" {
		t.Fatalf("itemType: got %q, want collab_agent", events[0].ItemType)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["item_status"] != "failed" {
		t.Fatalf("item_status = %v, want failed", meta["item_status"])
	}
	input, ok := meta["input"].(map[string]any)
	if !ok {
		t.Fatalf("input missing from meta: %+v", meta)
	}
	if input["prompt"] != "Spawn beyond the limit" {
		t.Fatalf("prompt: got %v", input["prompt"])
	}
}

func TestClassifyNotification_CollabSendInputUsesDedicatedType(t *testing.T) {
	params := json.RawMessage(`{"item":{"id":"call-2","type":"collabAgentToolCall","tool":"sendInput","prompt":"continue","receiverThreadIds":["child-1"],"status":"completed"}}`)
	events := ClassifyNotification("t1", "item/completed", params)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].ItemType != "send_input" {
		t.Fatalf("itemType: got %q, want %q", events[0].ItemType, "send_input")
	}
}

// TestClassifyNotification_CollabSpawnSurfacesAgentsStates is the
// primary user of the agentsStates enrichment: the parent spawn_agent
// card tracks each child thread's live status so the UI can render
// "agent running…" / "agent completed" badges without subscribing to
// every child thread's session status. Codex emits `agentsStates` on
// `item/completed` (CollabAgentSpawnEnd in codex-source/.../app-server/
// src/bespoke_event_handling.rs) — there is no `item/updated` method.
func TestClassifyNotification_CollabSpawnSurfacesAgentsStates(t *testing.T) {
	params := json.RawMessage(
		`{"item":{"id":"call-1","type":"collabAgentToolCall","tool":"spawnAgent",` +
			`"prompt":"Refactor auth","receiverThreadIds":["child-1"],"status":"completed",` +
			`"agentsStates":{"child-1":{"status":"running"}}}}`,
	)
	events := ClassifyNotification("t1", "item/completed", params)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	input, ok := meta["input"].(map[string]any)
	if !ok {
		t.Fatalf("input missing from meta: %+v", meta)
	}
	states, ok := input["agentsStates"].(map[string]any)
	if !ok {
		t.Fatalf("agentsStates missing from input: %+v", input)
	}
	child, ok := states["child-1"].(map[string]any)
	if !ok {
		t.Fatalf("child-1 entry wrong shape: %+v", states)
	}
	if child["status"] != "running" {
		t.Errorf("child-1 status: got %v, want %q", child["status"], "running")
	}
}

// TestClassifyNotification_CollabSpawnWithoutAgentsStatesOmitsKey
// guards the "only include when populated" rule. A spawn_agent
// envelope that carries no agentsStates (or an empty object) must not
// bake an empty map into Meta — a missing key is distinct from a map
// that truly reports zero children, and the frontend's conditional
// rendering relies on the distinction.
func TestClassifyNotification_CollabSpawnWithoutAgentsStatesOmitsKey(t *testing.T) {
	params := json.RawMessage(
		`{"item":{"id":"call-1","type":"collabAgentToolCall","tool":"spawnAgent",` +
			`"prompt":"kick off","receiverThreadIds":["child-1"],"status":"completed"}}`,
	)
	events := ClassifyNotification("t1", "item/completed", params)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	input, _ := meta["input"].(map[string]any)
	if _, present := input["agentsStates"]; present {
		t.Errorf("agentsStates should be absent when wire payload omits it; input=%+v", input)
	}
}

// TestClassifyNotification_CollabWaitEmitsWaitAgent verifies that the
// `wait` collab tool emits a dedicated `wait_agent` item type so the UI
// can render a distinct "waiting on N agents" card. The wire value is
// `"wait"` per codex-source v2.rs:4977 (`CollabAgentTool::Wait` with
// `rename_all = "camelCase"` — single-word enums serialize verbatim).
// The older `"waitAgent"` variant is never emitted by a live server but
// is accepted as a defensive alias.
func TestClassifyNotification_CollabWaitEmitsWaitAgent(t *testing.T) {
	cases := []struct {
		name string
		tool string
	}{
		{"canonical wire value", "wait"},
		{"defensive alias waitAgent", "waitAgent"},
		{"defensive alias wait_agent", "wait_agent"},
		{"title-case legacy", "WaitAgent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := json.RawMessage(
				`{"item":{"id":"call-3","type":"collabAgentToolCall","tool":"` + tc.tool +
					`","receiverThreadIds":["child-1"],"status":"completed"}}`,
			)
			events := ClassifyNotification("t1", "item/completed", params)
			if len(events) != 1 {
				t.Fatalf("expected 1 event for tool=%q, got %d", tc.tool, len(events))
			}
			evt := events[0]
			if evt.Kind != provider.EventToolComplete {
				t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventToolComplete)
			}
			if evt.ItemType != "wait_agent" {
				t.Errorf("itemType for tool=%q: got %q, want %q", tc.tool, evt.ItemType, "wait_agent")
			}
			var meta map[string]any
			if err := json.Unmarshal(evt.Meta, &meta); err != nil {
				t.Fatalf("unmarshal meta: %v", err)
			}
			if meta["toolName"] != "wait_agent" {
				t.Errorf("toolName: got %v, want %q", meta["toolName"], "wait_agent")
			}
		})
	}
}

// TestClassifyNotification_CollabWaitSurfacesAgentsStates pins that the
// parent wait card carries the agentsStates map (child thread ID →
// {status, message?}) into its Meta. The v1 wait tool populates this on
// the end event so the UI can show per-child terminal status on the
// same card the agent blocked on. See codex-wire.md §Collab agent
// lifecycle and codex-source v2.rs:4462 (`agents_states`).
func TestClassifyNotification_CollabWaitSurfacesAgentsStates(t *testing.T) {
	params := json.RawMessage(
		`{"item":{"id":"call-3","type":"collabAgentToolCall","tool":"wait",` +
			`"receiverThreadIds":["child-1","child-2"],"status":"completed",` +
			`"agentsStates":{` +
			`"child-1":{"status":"completed","message":"ok"},` +
			`"child-2":{"status":"errored","message":"boom"}` +
			`}}}`,
	)
	events := ClassifyNotification("t1", "item/completed", params)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	input, ok := meta["input"].(map[string]any)
	if !ok {
		t.Fatalf("input missing from meta: %+v", meta)
	}
	states, ok := input["agentsStates"].(map[string]any)
	if !ok {
		t.Fatalf("agentsStates missing from input: %+v", input)
	}
	// Spot-check one child; the whole nested object must round-trip.
	child1, ok := states["child-1"].(map[string]any)
	if !ok {
		t.Fatalf("child-1 entry wrong shape: %+v", states)
	}
	if child1["status"] != "completed" {
		t.Errorf("child-1 status: got %v, want %q", child1["status"], "completed")
	}
	if child1["message"] != "ok" {
		t.Errorf("child-1 message: got %v, want %q", child1["message"], "ok")
	}
}

// TestClassifyNotification_UnknownMethodDoesNotCrash guards the
// default-skip branch for a hypothetical Codex method we don't handle
// yet. It must not panic and must emit no events.
func TestClassifyNotification_UnknownMethodDoesNotCrash(t *testing.T) {
	events := ClassifyNotification("t1", "item/futurefeature", json.RawMessage(`{"whatever":true}`))
	if len(events) != 0 {
		t.Errorf("unknown method emitted %d events, want 0", len(events))
	}
}

// -- item/started surface tests (task #4: propagate CommandExecutionSource) --

// TestClassifyNotification_ItemStartedCommandExecutionSource verifies each
// of the four CommandExecutionSource variants (wire values from
// codex-source/schema/typescript/v2/CommandExecutionSource.ts) land at the
// top of evt.Meta as the `source` key. Downstream classifiers branch on
// source (unifiedExec is the hint that a command will keep running after
// its tool-call "completes" on the wire); nested access into
// meta.item.source works today but is fragile to a future schema bump.
func TestClassifyNotification_ItemStartedCommandExecutionSource(t *testing.T) {
	sources := []string{"agent", "userShell", "unifiedExecStartup", "unifiedExecInteraction"}
	for _, source := range sources {
		t.Run(source, func(t *testing.T) {
			params := json.RawMessage(
				`{"item":{"id":"i1","type":"commandExecution","source":"` + source + `","status":"inProgress"}}`,
			)
			events := ClassifyNotification("t1", "item/started", params)
			if len(events) != 1 {
				t.Fatalf("expected 1 event, got %d", len(events))
			}
			evt := events[0]
			if evt.Kind != provider.EventToolStart {
				t.Fatalf("kind: got %q, want %q", evt.Kind, provider.EventToolStart)
			}
			var meta map[string]any
			if err := json.Unmarshal(evt.Meta, &meta); err != nil {
				t.Fatalf("unmarshal meta: %v", err)
			}
			if meta["source"] != source {
				t.Errorf("meta.source: got %v, want %q (meta=%+v)", meta["source"], source, meta)
			}
			if meta["item_status"] != "inProgress" {
				t.Errorf("meta.item_status: got %v, want %q (meta=%+v)", meta["item_status"], "inProgress", meta)
			}
			// Nested item block must still be present — callers who
			// previously parsed meta.item.* should not break.
			if _, ok := meta["item"]; !ok {
				t.Errorf("meta.item was dropped by enrichment: %+v", meta)
			}
		})
	}
}

// TestClassifyNotification_ItemCompletedStatusFailed verifies failed-status
// command completions carry item_status into Meta. The failed status is the
// load-bearing signal for rendering a red glyph / stderr-forward UI.
func TestClassifyNotification_ItemCompletedStatusFailed(t *testing.T) {
	params := json.RawMessage(
		`{"item":{"id":"cmd-1","type":"commandExecution","source":"agent","status":"failed","exitCode":1}}`,
	)
	events := ClassifyNotification("t1", "item/completed", params)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	evt := events[0]
	if evt.Kind != provider.EventToolComplete {
		t.Fatalf("kind: got %q, want %q", evt.Kind, provider.EventToolComplete)
	}
	var meta map[string]any
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["item_status"] != "failed" {
		t.Errorf("meta.item_status: got %v, want %q (meta=%+v)", meta["item_status"], "failed", meta)
	}
	if meta["source"] != "agent" {
		t.Errorf("meta.source: got %v, want %q (meta=%+v)", meta["source"], "agent", meta)
	}
}

// TestClassifyNotification_ItemCompletedEachStatus covers each
// CommandExecutionStatus variant so a schema rename in codex-source
// surfaces here instead of silently dropping.
func TestClassifyNotification_ItemCompletedEachStatus(t *testing.T) {
	statuses := []string{"inProgress", "completed", "failed", "declined"}
	for _, status := range statuses {
		t.Run(status, func(t *testing.T) {
			params := json.RawMessage(
				`{"item":{"id":"cmd-1","type":"commandExecution","source":"agent","status":"` + status + `"}}`,
			)
			events := ClassifyNotification("t1", "item/completed", params)
			if len(events) != 1 {
				t.Fatalf("expected 1 event, got %d", len(events))
			}
			var meta map[string]any
			if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
				t.Fatalf("unmarshal meta: %v", err)
			}
			if meta["item_status"] != status {
				t.Errorf("meta.item_status: got %v, want %q", meta["item_status"], status)
			}
		})
	}
}

// -- progress notifications dropped --

// TestClassifyNotification_TerminalInteractionEmptyStdin pins the
// empty-stdin variant of `item/commandExecution/terminalInteraction`: the
// Codex model called `write_stdin` with no input to poll a backgrounded
// PTY. The event must carry the thread/turn/item routing fields plus an
// empty Content so the Phase 6 triage handler can persist a "Waited for
// background terminal" row. Meta carries the process id for debugging.
func TestClassifyNotification_TerminalInteractionEmptyStdin(t *testing.T) {
	params := json.RawMessage(
		`{"threadId":"th-1","turnId":"turn-2","itemId":"cmd-1","processId":"pid-42","stdin":""}`,
	)
	events := ClassifyNotification("th-1", "item/commandExecution/terminalInteraction", params)
	if len(events) != 1 {
		t.Fatalf("expected 1 event for empty-stdin terminalInteraction, got %d", len(events))
	}
	evt := events[0]
	if evt.Kind != provider.EventTerminalInteraction {
		t.Errorf("Kind = %q, want %q", evt.Kind, provider.EventTerminalInteraction)
	}
	if evt.ThreadID != "th-1" {
		t.Errorf("ThreadID = %q, want th-1", evt.ThreadID)
	}
	if evt.TurnID != "turn-2" {
		t.Errorf("TurnID = %q, want turn-2", evt.TurnID)
	}
	if evt.ItemID != "cmd-1" {
		t.Errorf("ItemID = %q, want cmd-1", evt.ItemID)
	}
	if evt.Content != "" {
		t.Errorf("Content = %q, want empty string (polling variant)", evt.Content)
	}
	var meta map[string]any
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["process_id"] != "pid-42" {
		t.Errorf("meta.process_id = %v, want pid-42", meta["process_id"])
	}
	if meta["stdin"] != "" {
		t.Errorf("meta.stdin = %v, want empty string", meta["stdin"])
	}
}

func TestClassifyNotification_RawResponseWriteStdinCallDropped(t *testing.T) {
	params := json.RawMessage(
		`{"threadId":"th-1","turnId":"turn-2","item":{"id":"fc-1","type":"function_call","name":"write_stdin","call_id":"call-stdin","arguments":"{\"session_id\":17313,\"chars\":\"\",\"yield_time_ms\":1000}"}}`,
	)
	events := ClassifyNotification("th-1", "rawResponseItem/completed", params)
	if len(events) != 0 {
		t.Fatalf("expected empty write_stdin raw response to stay non-visual, got %d", len(events))
	}
}

func TestClassifyNotification_RawResponseWriteStdinWithInputDropped(t *testing.T) {
	params := json.RawMessage(
		`{"threadId":"th-1","turnId":"turn-2","item":{"id":"fc-1","type":"function_call","name":"write_stdin","call_id":"call-stdin","arguments":"{\"session_id\":\"pid-42\",\"chars\":\"secret\\n\"}"}}`,
	)
	events := ClassifyNotification("th-1", "rawResponseItem/completed", params)
	if len(events) != 0 {
		t.Fatalf("expected non-empty write_stdin raw response to be dropped, got %d events", len(events))
	}
}

func TestClassifyNotification_RawResponseWriteStdinOutputDropped(t *testing.T) {
	params := json.RawMessage(
		`{"threadId":"th-1","turnId":"turn-2","item":{"type":"function_call_output","call_id":"call-stdin","rawToolName":"write_stdin","processId":"17313","waitResult":"running","output":"Chunk ID: x\nWall time: 1.0000 seconds\nProcess running with session ID 17313\nOutput:\n"}}`,
	)
	events := ClassifyNotification("th-1", "rawResponseItem/completed", params)
	if len(events) != 0 {
		t.Fatalf("expected write_stdin raw output to be dropped, got %d events", len(events))
	}
}

func TestClassifyNotification_RawSpawnAgentOutputDropped(t *testing.T) {
	params := json.RawMessage(
		`{"threadId":"th-1","turnId":"turn-2","item":{"type":"function_call_output","call_id":"spawn-1","rawToolName":"spawn_agent","output":"agent thread limit reached"}}`,
	)
	events := ClassifyNotification("th-1", "rawResponseItem/completed", params)
	if len(events) != 0 {
		t.Fatalf("expected raw spawn_agent output to stay non-visual, got %d events", len(events))
	}
}

// TestClassifyNotification_TerminalInteractionNonEmptyStdin pins the
// non-empty-stdin variant: the model forwarded actual keystrokes to the
// backgrounded PTY. The parser must STILL emit the event (rather than
// dropping it) so triage stays the single source of truth for "what
// renders in the timeline". Phase 6 triage drops the non-empty branch;
// future phases can render "Interacted with background terminal"
// without a parser change.
func TestClassifyNotification_TerminalInteractionNonEmptyStdin(t *testing.T) {
	params := json.RawMessage(
		`{"threadId":"th-1","turnId":"turn-2","itemId":"cmd-1","processId":"pid-42","stdin":"y\n"}`,
	)
	events := ClassifyNotification("th-1", "item/commandExecution/terminalInteraction", params)
	if len(events) != 1 {
		t.Fatalf("expected 1 event for non-empty-stdin terminalInteraction, got %d", len(events))
	}
	evt := events[0]
	if evt.Kind != provider.EventTerminalInteraction {
		t.Errorf("Kind = %q, want %q", evt.Kind, provider.EventTerminalInteraction)
	}
	if evt.Content != "y\n" {
		t.Errorf("Content = %q, want \"y\\n\" (stdin passthrough)", evt.Content)
	}
	var meta map[string]any
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["stdin"] != "y\n" {
		t.Errorf("meta.stdin = %v, want \"y\\n\"", meta["stdin"])
	}
}

func TestClassifyNotification_McpToolCallProgressDropped(t *testing.T) {
	params := json.RawMessage(
		`{"threadId":"th-1","turnId":"t1","itemId":"mcp-1","message":"Indexed 47/100 files"}`,
	)
	events := ClassifyNotification("th-1", "item/mcpToolCall/progress", params)
	if len(events) != 0 {
		t.Fatalf("expected mcpToolCall/progress to be dropped, got %d event(s)", len(events))
	}
}

func TestClassifyNotification_WebSearchEnrichesToolMeta(t *testing.T) {
	params := json.RawMessage(
		`{"threadId":"th-1","turnId":"t1","item":{"id":"web-1","type":"webSearch","query":"svelte 5 runes","status":"completed"}}`,
	)
	events := ClassifyNotification("th-1", "item/started", params)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["toolName"] != "WebSearch" {
		t.Fatalf("toolName = %v, want WebSearch", meta["toolName"])
	}
	input, ok := meta["input"].(map[string]any)
	if !ok {
		t.Fatalf("input missing or wrong type: %#v", meta["input"])
	}
	if input["query"] != "svelte 5 runes" {
		t.Fatalf("input.query = %v, want query", input["query"])
	}
}

func TestClassifyNotification_McpToolCallEnrichesToolMeta(t *testing.T) {
	params := json.RawMessage(
		`{"threadId":"th-1","turnId":"t1","item":{"id":"mcp-1","type":"mcpToolCall","server":"docs","tool":"lookup","arguments":{"q":"wails"},"durationMs":42,"status":"completed"}}`,
	)
	events := ClassifyNotification("th-1", "item/started", params)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["toolName"] != "MCP/lookup" {
		t.Fatalf("toolName = %v, want MCP/lookup", meta["toolName"])
	}
	if _, ok := meta["durationMs"]; ok {
		t.Fatalf("durationMs should not be surfaced until the UI has a persisted contract for it: %v", meta["durationMs"])
	}
	// The redesigned MCP row composes its body as `server.tool(args)`.
	// `meta.input` carries the raw arguments dict (same shape Claude's
	// parser produces) and `meta.mcp` carries the {server, tool} pair
	// the normalized toolName drops.
	input, ok := meta["input"].(map[string]any)
	if !ok {
		t.Fatalf("input missing or wrong type: %#v", meta["input"])
	}
	if input["q"] != "wails" {
		t.Fatalf("input.q = %v, want wails", input["q"])
	}
	if _, has := input["description"]; has {
		t.Fatalf("input.description should be gone after the MCP redesign: %#v", input["description"])
	}
	mcp, ok := meta["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("meta.mcp missing or wrong type: %#v", meta["mcp"])
	}
	if mcp["server"] != "docs" {
		t.Fatalf("meta.mcp.server = %v, want docs", mcp["server"])
	}
	if mcp["tool"] != "lookup" {
		t.Fatalf("meta.mcp.tool = %v, want lookup", mcp["tool"])
	}
}

func TestClassifyNotification_McpToolCallCompletionCarriesResultContent(t *testing.T) {
	params := json.RawMessage(
		`{"threadId":"th-1","turnId":"t1","item":{"id":"mcp-1","type":"mcpToolCall","server":"docs","tool":"lookup","status":"completed","result":{"content":[{"type":"text","text":"Lookup result"}],"structuredContent":{"id":"123"}}}}`,
	)
	events := ClassifyNotification("th-1", "item/completed", params)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventToolComplete {
		t.Fatalf("kind = %q, want %q", events[0].Kind, provider.EventToolComplete)
	}
	if !strings.Contains(events[0].Content, "Lookup result") {
		t.Fatalf("content missing MCP text result: %q", events[0].Content)
	}
	if !strings.Contains(events[0].Content, `"id": "123"`) {
		t.Fatalf("content missing structured content: %q", events[0].Content)
	}
}

func TestClassifyNotification_DynamicToolCallCompletionCarriesContentItems(t *testing.T) {
	params := json.RawMessage(
		`{"threadId":"th-1","turnId":"t1","item":{"id":"dyn-1","type":"dynamicToolCall","namespace":"codex_app","tool":"lookup_ticket","status":"completed","contentItems":[{"type":"inputText","text":"Ticket is open"}]}}`,
	)
	events := ClassifyNotification("th-1", "item/completed", params)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Content != "Ticket is open" {
		t.Fatalf("content = %q, want dynamic tool output", events[0].Content)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["toolName"] != "lookup_ticket" {
		t.Fatalf("toolName = %v, want lookup_ticket", meta["toolName"])
	}
}

func TestClassifyNotification_CollabAgentCompletionCarriesAgentMessage(t *testing.T) {
	params := json.RawMessage(
		`{"threadId":"th-1","turnId":"t1","item":{"id":"wait-1","type":"collabAgentToolCall","tool":"wait","status":"completed","receiverThreadIds":["child-1"],"agentsStates":{"child-1":{"status":"completed","message":"Final child answer"}}}}`,
	)
	events := ClassifyNotification("th-1", "item/completed", params)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	evt := events[0]
	if evt.Kind != provider.EventToolComplete {
		t.Fatalf("kind = %q, want %q", evt.Kind, provider.EventToolComplete)
	}
	if evt.ItemType != "wait_agent" {
		t.Fatalf("itemType = %q, want wait_agent", evt.ItemType)
	}
	if evt.Content != "Final child answer" {
		t.Fatalf("content = %q, want child final answer", evt.Content)
	}
	var meta map[string]any
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["toolName"] != "wait_agent" {
		t.Fatalf("toolName = %v, want wait_agent", meta["toolName"])
	}
	input, ok := meta["input"].(map[string]any)
	if !ok {
		t.Fatalf("input missing or wrong type: %#v", meta["input"])
	}
	if _, ok := input["agentsStates"].(map[string]any); !ok {
		t.Fatalf("input.agentsStates missing or wrong type: %#v", input["agentsStates"])
	}
}

func TestClassifyNotification_SpawnAgentSurfacesPromptAndFinalMessage(t *testing.T) {
	params := json.RawMessage(
		`{"threadId":"th-1","turnId":"t1","item":{"id":"spawn-1","type":"collabAgentToolCall","tool":"spawnAgent","status":"completed","prompt":"Inspect the parser","model":"gpt-5.4","reasoningEffort":"high","receiverThreadIds":["child-1"],"agentsStates":{"child-1":{"status":"completed","message":"Parser looks fine"}}}}`,
	)
	events := ClassifyNotification("th-1", "item/completed", params)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	evt := events[0]
	if evt.ItemType != "collab_agent" {
		t.Fatalf("itemType = %q, want collab_agent", evt.ItemType)
	}
	if evt.Content != "Parser looks fine" {
		t.Fatalf("content = %q, want final subagent output", evt.Content)
	}
	var meta map[string]any
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	input, ok := meta["input"].(map[string]any)
	if !ok {
		t.Fatalf("input missing or wrong type: %#v", meta["input"])
	}
	if input["prompt"] != "Inspect the parser" {
		t.Fatalf("input.prompt = %v, want original spawn prompt", input["prompt"])
	}
}

func TestClassifyNotification_SendInputSurfacesPromptAndFinalMessage(t *testing.T) {
	params := json.RawMessage(
		`{"threadId":"th-1","turnId":"t1","item":{"id":"send-1","type":"collabAgentToolCall","tool":"sendInput","status":"completed","prompt":"Please inspect this follow-up","receiverThreadIds":["child-1"],"agentsStates":{"child-1":{"status":"completed","message":"Follow-up handled"}}}}`,
	)
	events := ClassifyNotification("th-1", "item/completed", params)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	evt := events[0]
	if evt.ItemType != "send_input" {
		t.Fatalf("itemType = %q, want send_input", evt.ItemType)
	}
	if evt.Content != "Follow-up handled" {
		t.Fatalf("content = %q, want final subagent output", evt.Content)
	}
	var meta map[string]any
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	input, ok := meta["input"].(map[string]any)
	if !ok {
		t.Fatalf("input missing or wrong type: %#v", meta["input"])
	}
	if input["prompt"] != "Please inspect this follow-up" {
		t.Fatalf("input.prompt = %v, want sent prompt", input["prompt"])
	}
}

func TestClassifyNotification_ImageToolsSurfaceUsefulMetadata(t *testing.T) {
	t.Run("image view path", func(t *testing.T) {
		params := json.RawMessage(
			`{"threadId":"th-1","turnId":"t1","item":{"id":"view-1","type":"imageView","path":"/tmp/screenshot.png"}}`,
		)
		events := ClassifyNotification("th-1", "item/started", params)
		if len(events) != 1 {
			t.Fatalf("expected 1 event, got %d", len(events))
		}
		var meta map[string]any
		if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
			t.Fatalf("unmarshal meta: %v", err)
		}
		if meta["toolName"] != "ViewImage" {
			t.Fatalf("toolName = %v, want ViewImage", meta["toolName"])
		}
		input, ok := meta["input"].(map[string]any)
		if !ok {
			t.Fatalf("input missing or wrong type: %#v", meta["input"])
		}
		if input["path"] != "/tmp/screenshot.png" {
			t.Fatalf("input.path = %v, want image path", input["path"])
		}
	})

	t.Run("image generation saved path", func(t *testing.T) {
		params := json.RawMessage(
			`{"threadId":"th-1","turnId":"t1","item":{"id":"img-1","type":"imageGeneration","status":"completed","revisedPrompt":"A quiet dashboard","result":"base64-image-data","savedPath":"/tmp/generated.png"}}`,
		)
		events := ClassifyNotification("th-1", "item/completed", params)
		if len(events) != 1 {
			t.Fatalf("expected 1 event, got %d", len(events))
		}
		if !strings.Contains(events[0].Content, "A quiet dashboard") {
			t.Fatalf("content missing revised prompt: %q", events[0].Content)
		}
		if !strings.Contains(events[0].Content, "/tmp/generated.png") {
			t.Fatalf("content missing saved path: %q", events[0].Content)
		}
		if strings.Contains(events[0].Content, "base64-image-data") {
			t.Fatalf("content must not expose raw image data: %q", events[0].Content)
		}
		var meta map[string]any
		if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
			t.Fatalf("unmarshal meta: %v", err)
		}
		if strings.Contains(string(events[0].Meta), "base64-image-data") {
			t.Fatalf("meta must not preserve raw image result bytes: %s", string(events[0].Meta))
		}
		if meta["toolName"] != "ImageGeneration" {
			t.Fatalf("toolName = %v, want ImageGeneration", meta["toolName"])
		}
	})
}

func TestClassifyNotification_WebSearchCompletionDoesNotInventResultContent(t *testing.T) {
	tests := []struct {
		name   string
		action string
	}{
		{
			name:   "matching search query",
			action: `{"type":"search","query":"codex app-server"}`,
		},
		{
			name:   "alternate search query",
			action: `{"type":"search","query":"codex protocol"}`,
		},
		{
			name:   "query list",
			action: `{"type":"search","queries":["codex protocol","codex app-server"]}`,
		},
		{
			name:   "open page",
			action: `{"type":"openPage","url":"https://example.com"}`,
		},
		{
			name:   "find in page",
			action: `{"type":"findInPage","url":"https://example.com","pattern":"needle"}`,
		},
		{
			name:   "other",
			action: `{"type":"other","extra":"value"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params := json.RawMessage(
				`{"threadId":"th-1","turnId":"t1","item":{"id":"web-1","type":"webSearch","query":"codex app-server","action":` + tt.action + `}}`,
			)
			events := ClassifyNotification("th-1", "item/completed", params)
			if len(events) != 1 {
				t.Fatalf("expected 1 event, got %d", len(events))
			}
			if events[0].Content != "" {
				t.Fatalf("webSearch action has no result body on the wire; got content %q", events[0].Content)
			}
			var meta map[string]any
			if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
				t.Fatalf("unmarshal meta: %v", err)
			}
			input, ok := meta["input"].(map[string]any)
			if !ok {
				t.Fatalf("input missing or wrong type: %#v", meta["input"])
			}
			if input["query"] != "codex app-server" {
				t.Fatalf("input.query = %v, want final query", input["query"])
			}
		})
	}
}

// -- turn/completed surface test --

// TestClassifyNotification_ItemStartedUnifiedExecCarriesProcessID
// verifies the unifiedExecStartup source wire signal lands in Meta
// alongside the process_id for downstream backgrounding projection.
// Both fields live on the `item` subobject on the wire (processId is
// rendered camelCase via the Rust `rename_all` attribute). The
// projector reads these top-level to decide "this command can be
// backgrounded" without walking nested JSON.
func TestClassifyNotification_ItemStartedUnifiedExecCarriesProcessID(t *testing.T) {
	params := json.RawMessage(
		`{"threadId":"th-1","turnId":"turn-42",` +
			`"item":{"id":"cmd-1","type":"commandExecution",` +
			`"source":"unifiedExecStartup","processId":"pid-12345",` +
			`"status":"inProgress"}}`,
	)
	events := ClassifyNotification("th-1", "item/started", params)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	evt := events[0]
	if evt.Kind != provider.EventToolStart {
		t.Fatalf("kind: got %q, want %q", evt.Kind, provider.EventToolStart)
	}
	if evt.TurnID != "turn-42" {
		t.Errorf("evt.TurnID: got %q, want %q (needed for projector turn correlation)", evt.TurnID, "turn-42")
	}
	var meta map[string]any
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["source"] != "unifiedExecStartup" {
		t.Errorf("meta.source: got %v, want %q", meta["source"], "unifiedExecStartup")
	}
	if meta["process_id"] != "pid-12345" {
		t.Errorf("meta.process_id: got %v, want %q", meta["process_id"], "pid-12345")
	}
}

// TestClassifyNotification_ItemStartedAgentSourceOmitsProcessID pins
// the "only include when populated" rule on process_id. An
// agent-sourced CommandExecution typically has no wire processId
// (buffered exec gets an internal id Codex doesn't expose); the Meta
// key must then be absent, not an empty string — a missing key is the
// load-bearing signal for "no PTY handle to track."
func TestClassifyNotification_ItemStartedAgentSourceOmitsProcessID(t *testing.T) {
	params := json.RawMessage(
		`{"threadId":"th-1","turnId":"turn-3",` +
			`"item":{"id":"cmd-2","type":"commandExecution",` +
			`"source":"agent","status":"inProgress"}}`,
	)
	events := ClassifyNotification("th-1", "item/started", params)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["source"] != "agent" {
		t.Errorf("meta.source: got %v, want %q", meta["source"], "agent")
	}
	if _, present := meta["process_id"]; present {
		t.Errorf("meta.process_id must be absent when wire omits it; got %v", meta["process_id"])
	}
}

// TestClassifyNotification_ItemCompletedCarriesTurnID pins the
// turnId propagation on item/completed. The projector uses evt.TurnID
// to mark which turn owns a completing item so the completion sibling
// row can still land at the current timeline tail even if that turn
// is long gone by completion time.
func TestClassifyNotification_ItemCompletedCarriesTurnID(t *testing.T) {
	params := json.RawMessage(
		`{"threadId":"th-1","turnId":"turn-99",` +
			`"item":{"id":"cmd-1","type":"commandExecution",` +
			`"source":"unifiedExecStartup","status":"completed","exitCode":0}}`,
	)
	events := ClassifyNotification("th-1", "item/completed", params)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].TurnID != "turn-99" {
		t.Errorf("evt.TurnID: got %q, want %q", events[0].TurnID, "turn-99")
	}
}

// TestClassifyNotification_TurnCompletedNormalizesStatus asserts that
// Codex-specific turn.status values are translated before they leave the
// provider adapter.
func TestClassifyNotification_TurnCompletedNormalizesStatus(t *testing.T) {
	tests := []struct {
		name         string
		params       string
		wantStop     string
		wantAborted  bool
		wantError    string
		wantEventErr bool
	}{
		{
			name:     "completed",
			params:   `{"threadId":"th-1","turn":{"id":"t1","items":[],"status":"completed","error":null,"startedAt":1777926299,"completedAt":1777926306,"durationMs":6637}}`,
			wantStop: "end_turn",
		},
		{
			name:        "interrupted",
			params:      `{"threadId":"th-1","turn":{"id":"t1","items":[],"status":"interrupted","error":null,"startedAt":1777926299,"completedAt":1777926301,"durationMs":2000}}`,
			wantStop:    "interrupted",
			wantAborted: true,
		},
		{
			name:         "failed with error message",
			params:       `{"threadId":"th-1","turn":{"id":"t1","items":[],"status":"failed","error":{"message":"boom"},"startedAt":1777926299,"completedAt":1777926301,"durationMs":2000}}`,
			wantStop:     "error",
			wantError:    "boom",
			wantEventErr: true,
		},
		{
			name:     "unknown status stays empty",
			params:   `{"threadId":"th-1","turn":{"id":"t1","items":[],"status":"inProgress","error":null,"startedAt":1777926299,"completedAt":null,"durationMs":null}}`,
			wantStop: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events := ClassifyNotification("th-1", "turn/completed", json.RawMessage(tt.params))
			var complete provider.ProviderEvent
			var sawError bool
			for _, e := range events {
				switch e.Kind {
				case provider.EventTurnComplete:
					complete = e
				case provider.EventError:
					sawError = true
					if e.Content != tt.wantError {
						t.Errorf("EventError content = %q, want %q", e.Content, tt.wantError)
					}
				}
			}
			if complete.Kind == "" {
				t.Fatalf("no EventTurnComplete in events=%+v", events)
			}
			meta, ok := complete.TurnComplete.(*provider.WireTurnCompleteMeta)
			if !ok || meta == nil {
				t.Fatalf("turn complete meta = %T, want *WireTurnCompleteMeta", complete.TurnComplete)
			}
			if meta.StopReason != tt.wantStop {
				t.Errorf("StopReason = %q, want %q", meta.StopReason, tt.wantStop)
			}
			if meta.Aborted != tt.wantAborted {
				t.Errorf("Aborted = %v, want %v", meta.Aborted, tt.wantAborted)
			}
			if meta.AssistantMessageID != "" {
				t.Errorf("AssistantMessageID = %q, want empty: Codex turn/completed has no assistant message id", meta.AssistantMessageID)
			}
			if meta.ErrorMessage != tt.wantError {
				t.Errorf("ErrorMessage = %q, want %q", meta.ErrorMessage, tt.wantError)
			}
			if sawError != tt.wantEventErr {
				t.Errorf("EventError presence = %v, want %v", sawError, tt.wantEventErr)
			}
		})
	}
}
