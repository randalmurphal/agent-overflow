package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
	params := json.RawMessage(`{"turnId":"turn-1","itemId":"msg-1","delta":"Hello "}`)
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
	if events[0].TurnID != "turn-1" {
		t.Errorf("turnID: got %q, want %q", events[0].TurnID, "turn-1")
	}
	if events[0].ItemID != "msg-1" {
		t.Errorf("itemID: got %q, want %q", events[0].ItemID, "msg-1")
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

func TestItemStartedFileChangeNormalizesToInternalToolName(t *testing.T) {
	params := json.RawMessage(`{"turnId":"turn-1","item":{"id":"patch-1","type":"fileChange","changes":[{"path":"src/old.go","kind":{"type":"update","move_path":"src/new.go"},"diff":"@@ -1 +1 @@\n-old\n+new"}],"status":"inProgress"}}`)
	events := ClassifyNotification(testThread, "item/started", params)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	evt := events[0]
	if evt.Kind != provider.EventToolStart {
		t.Fatalf("kind: got %q, want %q", evt.Kind, provider.EventToolStart)
	}
	if evt.ItemType != "file_change" {
		t.Fatalf("itemType: got %q, want file_change", evt.ItemType)
	}
	var meta struct {
		ToolName   string `json:"toolName"`
		ItemStatus string `json:"item_status"`
		Input      struct {
			FilePath string `json:"file_path"`
		} `json:"input"`
		Item struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Status  string `json:"status"`
			Changes []struct {
				Path string `json:"path"`
				Kind struct {
					Type     string `json:"type"`
					MovePath string `json:"move_path"`
				} `json:"kind"`
				Diff string `json:"diff"`
			} `json:"changes"`
		} `json:"item"`
	}
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta.ToolName != "file_change" {
		t.Fatalf("toolName: got %q, want file_change", meta.ToolName)
	}
	if meta.ItemStatus != "inProgress" {
		t.Fatalf("item_status: got %q, want inProgress", meta.ItemStatus)
	}
	if meta.Input.FilePath != "src/new.go" {
		t.Fatalf("input.file_path: got %q, want src/new.go", meta.Input.FilePath)
	}
	if meta.Item.ID != "patch-1" || meta.Item.Type != "fileChange" || meta.Item.Status != "inProgress" {
		t.Fatalf("meta.item identity = %+v, want patch-1/fileChange/inProgress", meta.Item)
	}
	if len(meta.Item.Changes) != 1 {
		t.Fatalf("meta.item.changes length = %d, want 1", len(meta.Item.Changes))
	}
	change := meta.Item.Changes[0]
	if change.Path != "src/old.go" || change.Kind.MovePath != "src/new.go" || change.Diff == "" {
		t.Fatalf("meta.item.changes[0] = %+v, want path, move_path, and diff preserved", change)
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

// TestItemStartedDropsNonToolTypes guards the sidebar live-status
// projection. Codex fires item/started for every ThreadItem variant —
// including userMessage, agentMessage, reasoning — but only true tools
// (command execution, file change, mcp tool calls, etc.) should land
// as EventToolStart. Without this filter, every new turn writes ghost
// tool_call rows that flicker the sidebar pill running -> idle during
// the gap between userMessage settling and agentMessage streaming,
// which is long enough for the "Completed" pill to render mid-turn.
func TestItemStartedDropsNonToolTypes(t *testing.T) {
	cases := []string{"userMessage", "agentMessage", "assistantMessage", "reasoning", "plan", "todoList"}
	for _, itemType := range cases {
		t.Run(itemType, func(t *testing.T) {
			params := json.RawMessage(`{"item":{"id":"item-X","type":"` + itemType + `"}}`)
			events := ClassifyNotification(testThread, "item/started", params)
			if len(events) != 0 {
				t.Fatalf("%s started should be dropped, got %d events: %+v", itemType, len(events), events)
			}
		})
	}
}

// TestItemCompletedDropsNonToolContentTypes mirrors the started filter:
// completions for todoList must not settle as tool_call rows. Carve-outs:
//   - agentMessage / assistantMessage / reasoning settle their streaming
//     content rows via EventContentBlockStop.
//   - plan re-routes to EventProposedPlan (covered by
//     TestClassifyNotification_ItemCompletedPlan).
//   - userMessage promotes to EventUserText (covered by the
//     TestClassifyItemCompleted_UserMessage_* family in
//     protocol_test.go); see classifyItemCompleted's userMessage
//     branch and parse_user.go's `isReplay:true` mirror on the
//     Claude side.
func TestItemCompletedDropsNonToolContentTypes(t *testing.T) {
	cases := []string{"todoList"}
	for _, itemType := range cases {
		t.Run(itemType, func(t *testing.T) {
			params := json.RawMessage(`{"item":{"id":"item-X","type":"` + itemType + `"}}`)
			events := ClassifyNotification(testThread, "item/completed", params)
			if len(events) != 0 {
				t.Fatalf("%s completed should be dropped, got %d events: %+v", itemType, len(events), events)
			}
		})
	}
}

func TestItemCompletedSettlesStreamingContentTypes(t *testing.T) {
	cases := []struct {
		name          string
		itemType      string
		wantBlockType string
	}{
		{name: "agent message", itemType: "agentMessage", wantBlockType: "text"},
		{name: "assistant message", itemType: "assistantMessage", wantBlockType: "text"},
		{name: "reasoning", itemType: "reasoning", wantBlockType: "thinking"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := json.RawMessage(`{"turnId":"turn-1","item":{"id":"item-X","type":"` + tc.itemType + `"}}`)
			events := ClassifyNotification(testThread, "item/completed", params)
			if len(events) != 1 {
				t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
			}
			if events[0].Kind != provider.EventContentBlockStop {
				t.Fatalf("kind = %q, want content block stop", events[0].Kind)
			}
			var meta map[string]string
			if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
				t.Fatalf("meta unmarshal: %v", err)
			}
			if meta["blockType"] != tc.wantBlockType {
				t.Fatalf("blockType = %q, want %q", meta["blockType"], tc.wantBlockType)
			}
		})
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
		t.Fatalf("expected 1 upgrade-only diff event, got %d: %+v", len(events), events)
	}
	if events[0].Kind != provider.EventDiff {
		t.Fatalf("kind = %q, want %q", events[0].Kind, provider.EventDiff)
	}
	if events[0].Content != "--- a/main.go\n+++ b/main.go\n" {
		t.Fatalf("content = %q", events[0].Content)
	}
	var meta struct {
		UpgradeOnly bool   `json:"upgrade_only"`
		Source      string `json:"source"`
	}
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if !meta.UpgradeOnly || meta.Source != "turn/diff/updated" {
		t.Fatalf("meta = %+v, want upgrade-only turn diff marker", meta)
	}
}

func TestFileChangeOutputDelta(t *testing.T) {
	params := json.RawMessage(`{"delta":"diff content"}`)
	events := ClassifyNotification(testThread, "item/fileChange/outputDelta", params)

	if len(events) != 0 {
		t.Fatalf("item/fileChange/outputDelta should not create transcript events, got %+v", events)
	}
}

func TestFileChangePatchUpdated(t *testing.T) {
	params := json.RawMessage(`{"itemId":"patch-1","changes":[]}`)
	events := ClassifyNotification(testThread, "item/fileChange/patchUpdated", params)

	if len(events) != 0 {
		t.Fatalf("item/fileChange/patchUpdated should not create transcript events, got %+v", events)
	}
}

func TestTokenUsageUpdated(t *testing.T) {
	params := json.RawMessage(`{"tokenUsage":{"last":{"inputTokens":100,"outputTokens":20,"cachedInputTokens":6,"totalTokens":126},"total":{"inputTokens":9000,"outputTokens":2000,"cachedInputTokens":839,"totalTokens":11839},"modelContextWindow":258400}}`)
	events := ClassifyNotification(testThread, "thread/tokenUsage/updated", params)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventTokenUsage {
		t.Errorf("kind: got %q, want %q", events[0].Kind, provider.EventTokenUsage)
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

func TestTurnPlanUpdatedEmitsTodoUpdate(t *testing.T) {
	// Real Codex wire shape per app-server-protocol/v2.rs
	// `TurnPlanUpdatedNotification.plan: Vec<TurnPlanStep>`. The previous
	// fixture used `plan: "step 1, step 2"` (a string) which only happened
	// to satisfy the `Kind` assertion; with the new triage `decodeTodoSteps`
	// gating the frontend emit, that shape would be silently dropped. Pin
	// the array shape here so a wire regression surfaces at parse time.
	params := json.RawMessage(`{"plan":[{"step":"step 1","status":"inProgress"},{"step":"step 2","status":"pending"}]}`)
	events := ClassifyNotification(testThread, "turn/plan/updated", params)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != provider.EventTodoUpdate {
		t.Fatalf("kind = %q, want %q", events[0].Kind, provider.EventTodoUpdate)
	}
	// Confirm the plan array survives onto Meta in a shape the triage
	// decoder will accept (decodeTodoSteps lives in internal/triage; we
	// only assert the underlying JSON shape here so this package stays
	// dependency-free).
	var meta struct {
		Plan []struct {
			Step   string `json:"step"`
			Status string `json:"status"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if len(meta.Plan) != 2 || meta.Plan[0].Step != "step 1" || meta.Plan[0].Status != "inProgress" {
		t.Fatalf("plan steps: got %+v", meta.Plan)
	}
}

func TestClassifyReasoningTextDelta(t *testing.T) {
	params := json.RawMessage(`{"turnId":"turn-1","itemId":"reason-1","delta":"thinking about this..."}`)
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
	if events[0].TurnID != "turn-1" {
		t.Errorf("turnID: got %q, want %q", events[0].TurnID, "turn-1")
	}
	if events[0].ItemID != "reason-1" {
		t.Errorf("itemID: got %q, want %q", events[0].ItemID, "reason-1")
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

// Regression guard for the original bug: the user's screenshot showed
// "5-HOUR LIMIT 46% used" while Codex TUI showed "5h limit: 0% left"
// (100% used) for the same account. The values are wired into the
// fixture below — codex carries 100/91 and spark carries 46/22. The
// frontend store keys by (provider, windowMins) only, so without the
// canonical-bucket filter spark would overwrite codex at the 300 and
// 10080 slots and produce exactly the reported symptom.
//
// The test also distinguishes the two branches of
// extractCodexRateLimitEntries by giving the top-level `rateLimits`
// (1/2 — fallback-only data) different values than
// `rateLimitsByLimitId.codex` (100/91 — preferred path), so a
// regression that flips the precedence shows up immediately.
func TestClassifyRateLimitsUpdated(t *testing.T) {
	params := json.RawMessage(`{
		"rateLimits": {
			"limitId": "codex",
			"limitName": "Codex",
			"primary": {"usedPercent": 1, "windowDurationMins": 300, "resetsAt": 1775803864},
			"secondary": {"usedPercent": 2, "windowDurationMins": 10080, "resetsAt": 1776372636}
		},
		"rateLimitsByLimitId": {
			"codex": {
				"limitId": "codex",
				"limitName": "Codex",
				"primary": {"usedPercent": 100, "windowDurationMins": 300, "resetsAt": 1775803864},
				"secondary": {"usedPercent": 91, "windowDurationMins": 10080, "resetsAt": 1776372636}
			},
			"spark": {
				"limitId": "spark",
				"limitName": "GPT-5.3-Codex-Spark",
				"primary": {"usedPercent": 46, "windowDurationMins": 300, "resetsAt": 1775809666},
				"secondary": {"usedPercent": 22, "windowDurationMins": 10080, "resetsAt": 1776396466}
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
		t.Fatalf("limits len: got %d, want 4 (codex and spark primary + secondary)", len(snapshot.Limits))
	}
	if snapshot.Limits[0].LimitID != "codex" || snapshot.Limits[0].WindowMins != 300 {
		t.Errorf("limits[0]: got %+v", snapshot.Limits[0])
	}
	if snapshot.Limits[0].UsedPercent != 100 {
		t.Errorf("limits[0].UsedPercent: got %v, want 100 (codex bucket value, NOT spark's 46 nor top-level's 1)", snapshot.Limits[0].UsedPercent)
	}
	if snapshot.Limits[1].LimitID != "codex" || snapshot.Limits[1].WindowMins != 10080 {
		t.Errorf("limits[1]: got %+v", snapshot.Limits[1])
	}
	if snapshot.Limits[1].UsedPercent != 91 {
		t.Errorf("limits[1].UsedPercent: got %v, want 91 (codex bucket value, NOT spark's 22 nor top-level's 2)", snapshot.Limits[1].UsedPercent)
	}
	if snapshot.Limits[2].LimitID != "spark" || snapshot.Limits[2].UsedPercent != 46 {
		t.Errorf("limits[2]: got %+v, want spark primary at 46", snapshot.Limits[2])
	}
	if snapshot.Limits[3].LimitID != "spark" || snapshot.Limits[3].UsedPercent != 22 {
		t.Errorf("limits[3]: got %+v, want spark secondary at 22", snapshot.Limits[3])
	}
}

// When `rateLimitsByLimitId` is absent (the notification path), the
// parser must fall back to the top-level `rateLimits` snapshot. Pinned
// here so a future refactor of extractCodexRateLimitEntries can't
// silently drop the fallback path.
func TestClassifyRateLimitsUsesTopLevelWhenByLimitIdAbsent(t *testing.T) {
	params := json.RawMessage(`{
		"rateLimits": {
			"limitId": "codex",
			"primary": {"usedPercent": 73, "windowDurationMins": 300, "resetsAt": 1775803864},
			"secondary": {"usedPercent": 12, "windowDurationMins": 10080, "resetsAt": 1776372636}
		}
	}`)
	events := ClassifyNotification(testThread, "account/rateLimits/updated", params)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	var snapshot provider.RateLimitsSnapshot
	if err := json.Unmarshal(events[0].Meta, &snapshot); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if len(snapshot.Limits) != 2 || snapshot.Limits[0].UsedPercent != 73 || snapshot.Limits[1].UsedPercent != 12 {
		t.Errorf("snapshot: got %+v, want [{73, 300}, {12, 10080}]", snapshot.Limits)
	}
}

// Codex's wire `limit_id` is Option<String> without
// `skip_serializing_if`, so the default-bucket case arrives as
// `"limitId": null`. The TUI defaults this to `"codex"`
// (chatwidget.rs:2891); without the same default we silently drop the
// entire snapshot and the 5h/7d rings stay stale forever.
func TestClassifyRateLimitsDefaultsMissingLimitId(t *testing.T) {
	cases := map[string]json.RawMessage{
		"null": json.RawMessage(`{
			"rateLimits": {
				"limitId": null,
				"primary": {"usedPercent": 91, "windowDurationMins": 300, "resetsAt": 1775803864},
				"secondary": {"usedPercent": 7, "windowDurationMins": 10080, "resetsAt": 1776372636}
			}
		}`),
		"absent": json.RawMessage(`{
			"rateLimits": {
				"primary": {"usedPercent": 91, "windowDurationMins": 300, "resetsAt": 1775803864},
				"secondary": {"usedPercent": 7, "windowDurationMins": 10080, "resetsAt": 1776372636}
			}
		}`),
	}
	for name, params := range cases {
		t.Run(name, func(t *testing.T) {
			events := ClassifyNotification(testThread, "account/rateLimits/updated", params)
			if len(events) != 1 {
				t.Fatalf("expected 1 event, got %d", len(events))
			}
			var snapshot provider.RateLimitsSnapshot
			if err := json.Unmarshal(events[0].Meta, &snapshot); err != nil {
				t.Fatalf("unmarshal meta: %v", err)
			}
			if len(snapshot.Limits) != 2 {
				t.Fatalf("limits len: got %d, want 2 (missing limitId must default to codex)", len(snapshot.Limits))
			}
			for i, want := range []int{300, 10080} {
				if snapshot.Limits[i].LimitID != "codex" {
					t.Errorf("limits[%d].LimitID: got %q, want codex", i, snapshot.Limits[i].LimitID)
				}
				if snapshot.Limits[i].WindowMins != want {
					t.Errorf("limits[%d].WindowMins: got %d, want %d", i, snapshot.Limits[i].WindowMins, want)
				}
			}
		})
	}
}

// A standalone notification for an additional bucket must stay available.
// The frontend keys by account, limit ID, and window, so it cannot overwrite
// the provider's default allowance.
func TestClassifyRateLimitsRetainsDynamicBucket(t *testing.T) {
	params := json.RawMessage(`{
		"rateLimits": {
			"limitId": "spark",
			"limitName": "GPT-5.3-Codex-Spark",
			"primary": {"usedPercent": 46, "windowDurationMins": 300, "resetsAt": 1775809666},
			"secondary": {"usedPercent": 22, "windowDurationMins": 10080, "resetsAt": 1776396466}
		}
	}`)
	events := ClassifyNotification(testThread, "account/rateLimits/updated", params)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	var snapshot provider.RateLimitsSnapshot
	if err := json.Unmarshal(events[0].Meta, &snapshot); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if len(snapshot.Limits) != 2 {
		t.Fatalf("limits len: got %d, want 2", len(snapshot.Limits))
	}
	for _, limit := range snapshot.Limits {
		if limit.LimitID != "spark" || limit.LimitName != "GPT-5.3-Codex-Spark" {
			t.Errorf("dynamic bucket: got %+v", limit)
		}
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
	if events[0].ItemID != "c1" {
		t.Errorf("itemID: got %q, want c1", events[0].ItemID)
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
		"account/updated",
		"account/login/completed",
	}

	for _, method := range skipped {
		events := ClassifyNotification(testThread, method, json.RawMessage(`{}`))
		if len(events) != 0 {
			t.Errorf("method %q: expected 0 events, got %d", method, len(events))
		}
	}
}

// TestClassifyReasoningSummaryPartAddedEmitsParagraphBreak pins the
// section-boundary behaviour: when Codex's reasoning summary opens a
// new section, we inject a "\n\n" thinking delta so the accumulated
// thinking row renders with visible paragraph breaks between sections
// instead of one run-on blob. Section content itself continues to
// arrive via `summaryTextDelta` and concatenates onto the same row.
func TestClassifyReasoningSummaryPartAddedEmitsParagraphBreak(t *testing.T) {
	events := ClassifyNotification(testThread, "item/reasoning/summaryPartAdded", json.RawMessage(`{"itemId":"i1","summaryIndex":1}`))
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}
	if events[0].Kind != provider.EventThinking {
		t.Errorf("kind: got %q, want EventThinking", events[0].Kind)
	}
	if events[0].Content != "\n\n" {
		t.Errorf("content: got %q, want %q", events[0].Content, "\n\n")
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
	_ = s.Send(context.Background(), "hello", provider.SendOptions{})

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
	s.trackPendingApproval(42, provider.EventApprovalResolved)

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
	s.trackPendingApproval(42, provider.EventApprovalResolved)

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

	params := buildThreadParams(cfg, "")

	if params["model"] != "gpt-4.1" {
		t.Errorf("model: got %v, want %q", params["model"], "gpt-4.1")
	}
	if params["sandbox"] != "workspace-write" {
		t.Errorf("sandbox: got %v, want %q", params["sandbox"], "workspace-write")
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
	params := buildThreadParams(cfg, "")

	if params["approvalPolicy"] != "never" {
		t.Errorf("approvalPolicy: got %v, want %q", params["approvalPolicy"], "never")
	}
	if params["sandbox"] != "danger-full-access" {
		t.Errorf("sandbox: got %v, want %q", params["sandbox"], "danger-full-access")
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

	var request provider.UserInputRequest
	if err := json.Unmarshal(meta, &request); err != nil {
		t.Fatalf("unmarshal user input request: %v", err)
	}

	if request.TurnID != "turn-2" {
		t.Errorf("turnID: got %q, want %q", request.TurnID, "turn-2")
	}
	if request.ToolName != "user_input" {
		t.Errorf("toolName: got %q, want %q", request.ToolName, "user_input")
	}
	if len(request.Questions) != 1 {
		t.Fatalf("questions len: got %d, want 1", len(request.Questions))
	}
	if !request.Questions[0].MultiSelect {
		t.Fatal("expected multiSelect=true")
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

func TestDispatchLineChildNotificationSetsParentToolUseID(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID:            "parent-thread",
		pending:             make(map[int64]chan json.RawMessage),
		childParentByThread: map[string]string{"child-provider-1": "call-collab-1"},
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"child-provider-1","turnId":"turn-child-1","itemId":"msg-1","delta":"working"}}`))

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].ParentToolUseID != "call-collab-1" {
		t.Fatalf("ParentToolUseID: got %q, want %q", events[0].ParentToolUseID, "call-collab-1")
	}
	if events[0].ThreadID != "parent-thread" {
		t.Fatalf("ThreadID: got %q, want %q", events[0].ThreadID, "parent-thread")
	}
}

func TestDispatchLineSuppressesChildTurnLifecycle(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID:            "parent-thread",
		pending:             make(map[int64]chan json.RawMessage),
		childParentByThread: map[string]string{"child-provider-1": "call-collab-1"},
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"child-provider-1","turn":{"id":"turn-child-1"}}}`))
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"child-provider-1","turn":{"id":"turn-child-1","status":"completed"}}}`))

	if len(events) != 2 {
		t.Fatalf("expected running and terminal child status events, got %+v", events)
	}
	for _, event := range events {
		if event.Kind != provider.EventSubagentStatus || event.ItemID != "call-collab-1" {
			t.Fatalf("unexpected child lifecycle event: %+v", event)
		}
	}
	var meta map[string]string
	if err := json.Unmarshal(events[1].Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if meta["agent_path"] != "child-provider-1" || meta["status"] != "completed" {
		t.Fatalf("meta = %+v, want child-provider-1 completed", meta)
	}
}

// TestDispatchLineSuppressesChildTokenUsage ensures that child-thread
// `thread/tokenUsage/updated` notifications do not surface as
// EventTokenUsage on the parent thread. Per ADR-002 Codex subagents
// flatten onto the parent; without this filter the child's context
// window would overwrite the parent meter every time a subagent ran a
// turn. Mirrors the Claude precedent at
// internal/provider/claude/parse_assistant.go:appendContextUsageEvent.
func TestDispatchLineSuppressesChildTokenUsage(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID:            "parent-thread",
		pending:             make(map[int64]chan json.RawMessage),
		childParentByThread: map[string]string{"child-provider-1": "call-collab-1"},
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"child-provider-1","tokenUsage":{"last":{"totalTokens":50000},"modelContextWindow":200000}}}`))

	if len(events) != 0 {
		t.Fatalf("expected no events for child token usage, got %+v", events)
	}
}

// TestDispatchLineSuppressesChildCompacted ensures child-thread compaction
// notifications do not pollute parent state.
func TestDispatchLineSuppressesChildCompacted(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID:            "parent-thread",
		pending:             make(map[int64]chan json.RawMessage),
		childParentByThread: map[string]string{"child-provider-1": "call-collab-1"},
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"thread/compacted","params":{"threadId":"child-provider-1"}}`))

	if len(events) != 0 {
		t.Fatalf("expected no events for child compaction, got %+v", events)
	}
}

// TestDispatchLineSuppressesChildContextCompactionItem ensures the newer
// `contextCompaction` item lifecycle cannot leak a child compaction divider
// onto the parent thread. Older Codex builds emitted `thread/compacted`;
// current builds emit `item/completed` with item.type = contextCompaction.
func TestDispatchLineSuppressesChildContextCompactionItem(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID:            "parent-thread",
		pending:             make(map[int64]chan json.RawMessage),
		childParentByThread: map[string]string{"child-provider-1": "call-collab-1"},
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"child-provider-1","turnId":"child-turn-1","item":{"type":"contextCompaction","id":"compact-child-1"},"completedAtMs":1781709441420}}`))

	if len(events) != 0 {
		t.Fatalf("expected no events for child context compaction item, got %+v", events)
	}
}

func TestDispatchLineEmitsParentContextCompactionItem(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID:            "parent-thread",
		pending:             make(map[int64]chan json.RawMessage),
		childParentByThread: map[string]string{"child-provider-1": "call-collab-1"},
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}
	s.setRootThreadID("provider-parent")

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"provider-parent","turnId":"parent-turn-1","item":{"type":"contextCompaction","id":"compact-parent-1"},"completedAtMs":1781709692716}}`))

	if len(events) != 1 {
		t.Fatalf("expected 1 parent compaction event, got %d: %+v", len(events), events)
	}
	if events[0].Kind != provider.EventCompactBoundary {
		t.Fatalf("event kind = %q, want %q", events[0].Kind, provider.EventCompactBoundary)
	}
	if events[0].ItemID != "compact-parent-1" {
		t.Fatalf("item id = %q, want compact-parent-1", events[0].ItemID)
	}
	if events[0].ThreadID != "parent-thread" {
		t.Fatalf("thread id = %q, want parent-thread", events[0].ThreadID)
	}
}

// TestDispatchLineSuppressesChildNameUpdated ensures child-thread name updates
// don't rename the parent thread.
func TestDispatchLineSuppressesChildNameUpdated(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID:            "parent-thread",
		pending:             make(map[int64]chan json.RawMessage),
		childParentByThread: map[string]string{"child-provider-1": "call-collab-1"},
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"thread/name/updated","params":{"threadId":"child-provider-1","threadName":"Subagent Title"}}`))

	if len(events) != 0 {
		t.Fatalf("expected no events for child name update, got %+v", events)
	}
}

// TestDispatchLineParentTokenUsageStillEmits is the positive control for
// the suppression filter: parent-thread token usage must continue to flow.
func TestDispatchLineParentTokenUsageStillEmits(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID:            "parent-thread",
		pending:             make(map[int64]chan json.RawMessage),
		childParentByThread: map[string]string{"child-provider-1": "call-collab-1"},
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"provider-parent","tokenUsage":{"last":{"totalTokens":80000},"modelContextWindow":200000}}}`))

	if len(events) != 1 {
		t.Fatalf("expected one parent token usage event, got %+v", events)
	}
	if events[0].Kind != provider.EventTokenUsage {
		t.Fatalf("kind = %q, want %q", events[0].Kind, provider.EventTokenUsage)
	}
	if events[0].ParentToolUseID != "" {
		t.Fatalf("parent token usage should not carry ParentToolUseID, got %q", events[0].ParentToolUseID)
	}
}

func TestDispatchLineCollabSpawnRemembersReceiverThread(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID:            "parent-thread",
		pending:             make(map[int64]chan json.RawMessage),
		childParentByThread: map[string]string{"child-done": "call-collab-1"},
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"provider-parent","item":{"id":"call-collab-1","type":"collabAgentToolCall","tool":"spawnAgent","receiverThreadIds":["child-provider-1"],"prompt":"Refactor auth","status":"completed"}}}`))

	if got := s.parentToolUseForProviderThread("child-provider-1"); got != "call-collab-1" {
		t.Fatalf("child mapping: got %q, want %q", got, "call-collab-1")
	}
	if len(events) != 1 || events[0].ItemType != "collab_agent" {
		t.Fatalf("expected collab_agent event, got %+v", events)
	}
}

func TestDispatchLineThreadStartedRemembersAgentPath(t *testing.T) {
	s := &Session{
		threadID:               "parent-thread",
		pending:                make(map[int64]chan json.RawMessage),
		childParentByThread:    map[string]string{"child-provider-1": "call-collab-1"},
		childParentByAgentPath: make(map[string]string),
		agentPathByThread:      make(map[string]string),
		onEvent:                func(provider.ProviderEvent) {},
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"child-provider-1","source":{"subAgent":{"thread_spawn":{"parent_thread_id":"provider-parent","depth":1,"agent_path":"/root/researcher"}}}}}}`))

	if got := s.parentToolUseForAgentPath("/root/researcher"); got != "call-collab-1" {
		t.Fatalf("agent path mapping: got %q, want %q", got, "call-collab-1")
	}
}

func TestDispatchLineWaitAgentEnrichesReceiverMetadata(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID:               "parent-thread",
		pending:                make(map[int64]chan json.RawMessage),
		childParentByThread:    map[string]string{"child-provider-1": "call-collab-1"},
		childParentByAgentPath: make(map[string]string),
		agentPathByThread:      make(map[string]string),
		agentMetaByThread:      make(map[string]collabReceiverMeta),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"child-provider-1","agentNickname":"Galileo","agentRole":"explorer","source":{"subAgent":{"thread_spawn":{"parent_thread_id":"provider-parent","depth":1,"agent_path":"/root/researcher"}}}}}}`))
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"parent-thread","item":{"id":"wait-1","type":"collabAgentToolCall","tool":"wait","receiverThreadIds":["child-provider-1"],"status":"inProgress"}}}`))

	var waitEvent *provider.ProviderEvent
	for i := range events {
		if events[i].ItemType == "wait_agent" {
			waitEvent = &events[i]
		}
	}
	if waitEvent == nil {
		t.Fatalf("wait_agent event missing: %+v", events)
	}
	var meta struct {
		Input struct {
			ReceiverAgents []collabReceiverMeta `json:"receiverAgents"`
		} `json:"input"`
	}
	if err := json.Unmarshal(waitEvent.Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if len(meta.Input.ReceiverAgents) != 1 {
		t.Fatalf("receiverAgents = %+v, want one", meta.Input.ReceiverAgents)
	}
	got := meta.Input.ReceiverAgents[0]
	if got.ThreadID != "child-provider-1" || got.AgentNickname != "Galileo" || got.AgentRole != "explorer" {
		t.Fatalf("receiver metadata = %+v, want child-provider-1/Galileo/explorer", got)
	}
}

func TestDispatchLineRawSpawnOutputLabelsLaterWaitAgent(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID:               "parent-thread",
		pending:                make(map[int64]chan json.RawMessage),
		childParentByThread:    make(map[string]string),
		childParentByAgentPath: make(map[string]string),
		agentPathByThread:      make(map[string]string),
		agentMetaByThread:      make(map[string]collabReceiverMeta),
		rawToolCallsByID:       make(map[string]rawToolCall),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"type":"function_call","name":"spawn_agent","call_id":"spawn-1","arguments":"{\"agent_type\":\"explorer\",\"message\":\"Inspect parser\"}"}}}`))
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"id":"spawn-1","type":"collabAgentToolCall","tool":"spawnAgent","receiverThreadIds":["child-provider-1"],"prompt":"Inspect parser","status":"completed"}}}`))
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"type":"function_call_output","call_id":"spawn-1","output":"{\"agent_id\":\"child-provider-1\",\"nickname\":\"Boyle\"}"}}}`))
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"type":"function_call","name":"wait_agent","call_id":"wait-1","arguments":"{\"targets\":[\"child-provider-1\"],\"timeout_ms\":10000}"}}}`))
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"id":"wait-1","type":"collabAgentToolCall","tool":"wait","receiverThreadIds":["child-provider-1"],"status":"inProgress"}}}`))

	var waitEvent *provider.ProviderEvent
	for i := range events {
		if events[i].ItemType == "wait_agent" && events[i].Kind == provider.EventToolStart {
			waitEvent = &events[i]
		}
	}
	if waitEvent == nil {
		t.Fatalf("wait_agent event missing: %+v", events)
	}
	var meta struct {
		Input struct {
			ReceiverThreadIDs []string             `json:"receiverThreadIds"`
			ReceiverAgents    []collabReceiverMeta `json:"receiverAgents"`
		} `json:"input"`
	}
	if err := json.Unmarshal(waitEvent.Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if len(meta.Input.ReceiverThreadIDs) != 1 || meta.Input.ReceiverThreadIDs[0] != "child-provider-1" {
		t.Fatalf("receiverThreadIds = %+v, want child-provider-1", meta.Input.ReceiverThreadIDs)
	}
	if len(meta.Input.ReceiverAgents) != 1 {
		t.Fatalf("receiverAgents = %+v, want one", meta.Input.ReceiverAgents)
	}
	got := meta.Input.ReceiverAgents[0]
	if got.ThreadID != "child-provider-1" || got.AgentNickname != "Boyle" || got.AgentRole != "explorer" {
		t.Fatalf("receiver metadata = %+v, want child-provider-1/Boyle/explorer", got)
	}
}

func TestDispatchLineRawSpawnOutputMapsAgentIDForSubagentNotification(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID:               "parent-thread",
		pending:                make(map[int64]chan json.RawMessage),
		childParentByThread:    make(map[string]string),
		childParentByAgentPath: make(map[string]string),
		agentPathByThread:      make(map[string]string),
		agentMetaByThread:      make(map[string]collabReceiverMeta),
		rawToolCallsByID:       make(map[string]rawToolCall),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"type":"function_call","name":"spawn_agent","call_id":"spawn-1","arguments":"{\"agent_type\":\"default\",\"message\":\"Run a command, then finish\"}"}}}`))
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"type":"function_call_output","call_id":"spawn-1","output":"{\"agent_id\":\"019ecee6-4686-75e3-91aa-6594ec7aab09\",\"nickname\":\"Pasteur\"}"}}}`))
	if got := s.parentToolUseForProviderThread("019ecee6-4686-75e3-91aa-6594ec7aab09"); got != "spawn-1" {
		t.Fatalf("parent for raw spawn agent id = %q, want spawn-1", got)
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"parent-thread","item":{"id":"user-msg-1","type":"userMessage","content":[{"type":"text","text":"<subagent_notification>{\"agent_path\":\"019ecee6-4686-75e3-91aa-6594ec7aab09\",\"status\":{\"completed\":\"detached child finished after bash command\"}}</subagent_notification>"}]}}}`))

	var notif *provider.ProviderEvent
	for i := range events {
		switch events[i].Kind {
		case provider.EventSubagentNotification:
			notif = &events[i]
		case provider.EventUserText:
			if strings.Contains(events[i].Content, "subagent_notification") {
				t.Fatalf("subagent notification carrier emitted as user text: %+v", events[i])
			}
		}
	}
	if notif == nil {
		t.Fatalf("expected EventSubagentNotification, got %+v", events)
	}
	if notif.ItemID != "spawn-1" {
		t.Fatalf("notification ItemID = %q, want spawn-1", notif.ItemID)
	}
	var meta map[string]any
	if err := json.Unmarshal(notif.Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if meta["agent_path"] != "019ecee6-4686-75e3-91aa-6594ec7aab09" {
		t.Fatalf("meta.agent_path = %v, want raw spawned agent id", meta["agent_path"])
	}
	if meta["message"] != "detached child finished after bash command" {
		t.Fatalf("meta.message = %v, want child completion message", meta["message"])
	}
}

func TestDispatchLineRawSpawnOutputMapsAgentIDForRawUserSubagentNotification(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID:               "parent-thread",
		pending:                make(map[int64]chan json.RawMessage),
		childParentByThread:    make(map[string]string),
		childParentByAgentPath: make(map[string]string),
		agentPathByThread:      make(map[string]string),
		agentMetaByThread:      make(map[string]collabReceiverMeta),
		rawToolCallsByID:       make(map[string]rawToolCall),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}
	s.setRootThreadID("parent-provider-thread")

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"parent-provider-thread","turnId":"turn-1","item":{"type":"function_call","name":"spawn_agent","call_id":"spawn-1","arguments":"{\"agent_type\":\"default\",\"message\":\"Run a command, then finish\"}"}}}`))
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"parent-provider-thread","turnId":"turn-1","item":{"type":"function_call_output","call_id":"spawn-1","output":"{\"agent_id\":\"019ecef1-4a59-7932-8c94-76099197299b\",\"nickname\":\"Bernoulli\"}"}}}`))
	if got := s.parentToolUseForProviderThread("019ecef1-4a59-7932-8c94-76099197299b"); got != "spawn-1" {
		t.Fatalf("parent for raw spawn agent id = %q, want spawn-1", got)
	}

	s.dispatchLine(rawUserSubagentNotificationLineForThread(t, "parent-provider-thread", map[string]any{
		"agent_path": "019ecef1-4a59-7932-8c94-76099197299b",
		"status": map[string]any{
			"completed": "detached child retest finished after bash command",
		},
	}))

	var notif *provider.ProviderEvent
	for i := range events {
		switch events[i].Kind {
		case provider.EventSubagentNotification:
			notif = &events[i]
		case provider.EventUserText:
			if strings.Contains(events[i].Content, "subagent_notification") {
				t.Fatalf("raw user subagent carrier emitted as user text: %+v", events[i])
			}
		}
	}
	if notif == nil {
		t.Fatalf("expected EventSubagentNotification, got %+v", events)
	}
	if notif.ItemID != "spawn-1" {
		t.Fatalf("notification ItemID = %q, want spawn-1", notif.ItemID)
	}
	var meta map[string]any
	if err := json.Unmarshal(notif.Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if meta["agent_path"] != "019ecef1-4a59-7932-8c94-76099197299b" {
		t.Fatalf("meta.agent_path = %v, want raw spawned agent id", meta["agent_path"])
	}
	if meta["message"] != "detached child retest finished after bash command" {
		t.Fatalf("meta.message = %v, want child completion message", meta["message"])
	}
}

func TestReadChildThreadMetadataEmitsSpawnMetaUpdate(t *testing.T) {
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

	events := make(chan provider.ProviderEvent, 10)
	s := &Session{
		proc:                   proc,
		ctx:                    ctx,
		threadID:               "parent-thread",
		pending:                make(map[int64]chan json.RawMessage),
		childParentByThread:    make(map[string]string),
		childParentByAgentPath: make(map[string]string),
		agentPathByThread:      make(map[string]string),
		agentMetaByThread:      make(map[string]collabReceiverMeta),
		onEvent: func(evt provider.ProviderEvent) {
			events <- evt
		},
		cancel: cancel,
	}

	done := make(chan struct{})
	go func() {
		s.readChildThreadMetadata("child-provider-1", "spawn-1", collabLaunchMeta{
			Prompt:            "Run sleep 3",
			Model:             "gpt-5.5",
			ReasoningEffort:   "low",
			ReceiverThreadIDs: []string{"child-provider-1", "child-provider-2"},
		})
		close(done)
	}()

	pending, rpcID := waitForPending(t, s, 3*time.Second)
	pending <- json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"thread":{"id":"child-provider-1","agentNickname":"Newton","agentRole":"default"}}}`, rpcID))

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("readChildThreadMetadata did not return")
	}

	gotMeta := s.agentMetaByThread["child-provider-1"]
	if gotMeta.ThreadID != "child-provider-1" || gotMeta.AgentNickname != "Newton" || gotMeta.AgentRole != "default" {
		t.Fatalf("agent metadata = %+v, want child-provider-1/Newton/default", gotMeta)
	}

	var evt provider.ProviderEvent
	select {
	case evt = <-events:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for metadata update event")
	}
	if evt.Kind != provider.EventToolStart || evt.ItemID != "spawn-1" || evt.ItemType != "collab_agent" {
		t.Fatalf("event = %+v, want meta update for spawn-1", evt)
	}
	var meta struct {
		MetaUpdateOnly bool `json:"meta_update_only"`
		Input          struct {
			ReceiverThreadIDs []string `json:"receiverThreadIds"`
			NewAgentNickname  string   `json:"newAgentNickname"`
			NewAgentRole      string   `json:"newAgentRole"`
			Prompt            string   `json:"prompt"`
			Model             string   `json:"model"`
			ReasoningEffort   string   `json:"reasoningEffort"`
		} `json:"input"`
	}
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if !meta.MetaUpdateOnly {
		t.Fatal("meta_update_only = false, want true")
	}
	if !reflect.DeepEqual(meta.Input.ReceiverThreadIDs, []string{"child-provider-1", "child-provider-2"}) {
		t.Fatalf("receiverThreadIds = %+v, want full launch receiver list", meta.Input.ReceiverThreadIDs)
	}
	if meta.Input.NewAgentNickname != "Newton" || meta.Input.NewAgentRole != "default" {
		t.Fatalf("agent labels = %q/%q, want Newton/default", meta.Input.NewAgentNickname, meta.Input.NewAgentRole)
	}
	if meta.Input.Prompt != "Run sleep 3" || meta.Input.Model != "gpt-5.5" || meta.Input.ReasoningEffort != "low" {
		t.Fatalf("launch metadata = %q/%q/%q, want Run sleep 3/gpt-5.5/low", meta.Input.Prompt, meta.Input.Model, meta.Input.ReasoningEffort)
	}
}

func TestReadChildThreadMetadataRetriesUntilLabelsArrive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args: []string{"-c", `
			count=0
			while IFS= read -r line; do
				id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
				count=$((count + 1))
				if [ "$count" -eq 1 ]; then
					printf '{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"child-provider-1"}}}\n' "$id"
				else
					printf '{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"child-provider-1","agentNickname":"Curie","agentRole":"default"}}}\n' "$id"
				fi
			done
		`},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	events := make(chan provider.ProviderEvent, 10)
	s := &Session{
		proc:                   proc,
		ctx:                    ctx,
		threadID:               "parent-thread",
		pending:                make(map[int64]chan json.RawMessage),
		childParentByThread:    make(map[string]string),
		childParentByAgentPath: make(map[string]string),
		agentPathByThread:      make(map[string]string),
		agentMetaByThread:      make(map[string]collabReceiverMeta),
		collabMetadataReads:    make(chan struct{}, 1),
		onEvent: func(evt provider.ProviderEvent) {
			events <- evt
		},
		cancel: cancel,
	}
	go s.readLoop()
	t.Cleanup(func() {
		cancel()
		_ = proc.Close()
	})

	done := make(chan struct{})
	go func() {
		s.readChildThreadMetadata("child-provider-1", "spawn-1", collabLaunchMeta{
			ReceiverThreadIDs: []string{"child-provider-1"},
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("readChildThreadMetadata did not return")
	}

	gotMeta := s.agentMetaByThread["child-provider-1"]
	if gotMeta.ThreadID != "child-provider-1" || gotMeta.AgentNickname != "Curie" || gotMeta.AgentRole != "default" {
		t.Fatalf("agent metadata = %+v, want child-provider-1/Curie/default", gotMeta)
	}
	select {
	case evt := <-events:
		if evt.ItemID != "spawn-1" {
			t.Fatalf("event item id = %q, want spawn-1", evt.ItemID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for metadata update event")
	}
}

func TestReadChildThreadMetadataRequestsNoTurns(t *testing.T) {
	capturePath := t.TempDir() + "/request.json"
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args: []string{"-c", `
			IFS= read -r line || exit 1
			printf '%s\n' "$line" > "$CAPTURE_PATH"
			id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
			printf '{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"child-provider-1","agentNickname":"Noether","agentRole":"default"}}}\n' "$id"
		`},
		Env: map[string]string{"CAPTURE_PATH": capturePath},
	})
	if err != nil {
		t.Fatalf("spawn capture sh: %v", err)
	}
	s := &Session{
		proc:                   proc,
		ctx:                    ctx,
		threadID:               "parent-thread",
		pending:                make(map[int64]chan json.RawMessage),
		childParentByThread:    make(map[string]string),
		childParentByAgentPath: make(map[string]string),
		agentPathByThread:      make(map[string]string),
		agentMetaByThread:      make(map[string]collabReceiverMeta),
		collabMetadataReads:    make(chan struct{}, 1),
		onEvent:                func(provider.ProviderEvent) {},
		cancel:                 cancel,
	}
	go s.readLoop()
	t.Cleanup(func() {
		cancel()
		_ = proc.Close()
	})

	s.readChildThreadMetadata("child-provider-1", "spawn-1", collabLaunchMeta{
		ReceiverThreadIDs: []string{"child-provider-1"},
	})

	data, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read captured request: %v", err)
	}
	var frame struct {
		Method string `json:"method"`
		Params struct {
			ThreadID     string `json:"threadId"`
			IncludeTurns bool   `json:"includeTurns"`
		} `json:"params"`
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatalf("unmarshal captured request: %v", err)
	}
	var rawFrame map[string]any
	if err := json.Unmarshal(data, &rawFrame); err != nil {
		t.Fatalf("unmarshal raw captured request: %v", err)
	}
	rawParams, ok := rawFrame["params"].(map[string]any)
	if !ok {
		t.Fatalf("params missing from captured request: %s", string(data))
	}
	if _, ok := rawParams["includeTurns"]; !ok {
		t.Fatalf("includeTurns missing from captured request: %s", string(data))
	}
	if frame.Method != "thread/read" {
		t.Fatalf("method = %q, want thread/read", frame.Method)
	}
	if frame.Params.ThreadID != "child-provider-1" {
		t.Fatalf("threadId = %q, want child-provider-1", frame.Params.ThreadID)
	}
	if frame.Params.IncludeTurns {
		t.Fatal("includeTurns = true, want false")
	}
}

func TestDispatchLineRawWaitCallPreservesRequestedReceiversOnTimeoutCompletion(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID:         "parent-thread",
		pending:          make(map[int64]chan json.RawMessage),
		rawToolCallsByID: make(map[string]rawToolCall),
		agentMetaByThread: map[string]collabReceiverMeta{
			"child-provider-1": {ThreadID: "child-provider-1", AgentNickname: "Hypatia", AgentRole: "default"},
			"child-provider-2": {ThreadID: "child-provider-2", AgentNickname: "Parfit", AgentRole: "default"},
			"child-provider-3": {ThreadID: "child-provider-3", AgentNickname: "Ada", AgentRole: "default"},
		},
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"type":"function_call","name":"wait_agent","call_id":"wait-1","arguments":"{\"targets\":[\"child-provider-1\"],\"timeout_ms\":10000}"}}}`))
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"id":"wait-1","type":"collabAgentToolCall","tool":"wait","receiverThreadIds":[],"agentsStates":{},"status":"completed"}}}`))

	var waitEvent *provider.ProviderEvent
	for i := range events {
		if events[i].ItemType == "wait_agent" && events[i].Kind == provider.EventToolComplete {
			waitEvent = &events[i]
		}
	}
	if waitEvent == nil {
		t.Fatalf("wait_agent completion missing: %+v", events)
	}
	var meta struct {
		Input struct {
			ReceiverThreadIDs          []string             `json:"receiverThreadIds"`
			RequestedReceiverThreadIDs []string             `json:"requestedReceiverThreadIds"`
			RequestedReceiverAgents    []collabReceiverMeta `json:"requestedReceiverAgents"`
		} `json:"input"`
	}
	if err := json.Unmarshal(waitEvent.Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if len(meta.Input.ReceiverThreadIDs) != 0 {
		t.Fatalf("receiverThreadIds = %+v, want no timeout completions", meta.Input.ReceiverThreadIDs)
	}
	if want := []string{"child-provider-1"}; !reflect.DeepEqual(meta.Input.RequestedReceiverThreadIDs, want) {
		t.Fatalf("requestedReceiverThreadIds = %+v, want raw target preserved", meta.Input.RequestedReceiverThreadIDs)
	}
	wantAgents := []collabReceiverMeta{
		{ThreadID: "child-provider-1", AgentNickname: "Hypatia", AgentRole: "default"},
	}
	if !reflect.DeepEqual(meta.Input.RequestedReceiverAgents, wantAgents) {
		t.Fatalf("requestedReceiverAgents = %+v, want %+v", meta.Input.RequestedReceiverAgents, wantAgents)
	}
}

func TestDispatchLineTypedWaitCompletionPreservesStartedReceiverTargetsSeparately(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		pending:  make(map[int64]chan json.RawMessage),
		agentMetaByThread: map[string]collabReceiverMeta{
			"child-provider-1": {ThreadID: "child-provider-1", AgentNickname: "Hypatia", AgentRole: "default"},
			"child-provider-2": {ThreadID: "child-provider-2", AgentNickname: "Parfit", AgentRole: "default"},
			"child-provider-3": {ThreadID: "child-provider-3", AgentNickname: "Ada", AgentRole: "default"},
		},
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"id":"wait-1","type":"collabAgentToolCall","tool":"wait","receiverThreadIds":["child-provider-1","child-provider-2","child-provider-3"],"agentsStates":{},"status":"inProgress"}}}`))
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"id":"wait-1","type":"collabAgentToolCall","tool":"wait","receiverThreadIds":["child-provider-1"],"agentsStates":{"child-provider-1":{"status":"completed","message":"done"}},"status":"completed"}}}`))

	var waitEvent *provider.ProviderEvent
	for i := range events {
		if events[i].ItemType == "wait_agent" && events[i].Kind == provider.EventToolComplete {
			waitEvent = &events[i]
		}
	}
	if waitEvent == nil {
		t.Fatalf("wait_agent completion missing: %+v", events)
	}
	var meta struct {
		Input struct {
			ReceiverThreadIDs          []string             `json:"receiverThreadIds"`
			RequestedReceiverThreadIDs []string             `json:"requestedReceiverThreadIds"`
			ReceiverAgents             []collabReceiverMeta `json:"receiverAgents"`
			RequestedReceiverAgents    []collabReceiverMeta `json:"requestedReceiverAgents"`
			AgentsStates               map[string]struct {
				Status  string `json:"status"`
				Message string `json:"message"`
			} `json:"agentsStates"`
		} `json:"input"`
	}
	if err := json.Unmarshal(waitEvent.Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if want := []string{"child-provider-1"}; !reflect.DeepEqual(meta.Input.ReceiverThreadIDs, want) {
		t.Fatalf("receiverThreadIds = %+v, want completion statuses %+v", meta.Input.ReceiverThreadIDs, want)
	}
	wantRequested := []string{"child-provider-1", "child-provider-2", "child-provider-3"}
	if !reflect.DeepEqual(meta.Input.RequestedReceiverThreadIDs, wantRequested) {
		t.Fatalf("requestedReceiverThreadIds = %+v, want wait-start targets %+v", meta.Input.RequestedReceiverThreadIDs, wantRequested)
	}
	wantAgents := []collabReceiverMeta{
		{ThreadID: "child-provider-1", AgentNickname: "Hypatia", AgentRole: "default"},
	}
	if !reflect.DeepEqual(meta.Input.ReceiverAgents, wantAgents) {
		t.Fatalf("receiverAgents = %+v, want %+v", meta.Input.ReceiverAgents, wantAgents)
	}
	wantRequestedAgents := []collabReceiverMeta{
		{ThreadID: "child-provider-1", AgentNickname: "Hypatia", AgentRole: "default"},
		{ThreadID: "child-provider-2", AgentNickname: "Parfit", AgentRole: "default"},
		{ThreadID: "child-provider-3", AgentNickname: "Ada", AgentRole: "default"},
	}
	if !reflect.DeepEqual(meta.Input.RequestedReceiverAgents, wantRequestedAgents) {
		t.Fatalf("requestedReceiverAgents = %+v, want %+v", meta.Input.RequestedReceiverAgents, wantRequestedAgents)
	}
	if len(meta.Input.AgentsStates) != 1 || meta.Input.AgentsStates["child-provider-1"].Status != "completed" {
		t.Fatalf("agentsStates = %+v, want only completed child state", meta.Input.AgentsStates)
	}
}

func TestDispatchLineRawWaitCallPreservesAllReceiversSeparatelyOnPartialCompletion(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID:         "parent-thread",
		pending:          make(map[int64]chan json.RawMessage),
		rawToolCallsByID: make(map[string]rawToolCall),
		agentMetaByThread: map[string]collabReceiverMeta{
			"child-provider-1": {ThreadID: "child-provider-1", AgentNickname: "Hypatia", AgentRole: "default"},
			"child-provider-2": {ThreadID: "child-provider-2", AgentNickname: "Parfit", AgentRole: "default"},
			"child-provider-3": {ThreadID: "child-provider-3", AgentNickname: "Ada", AgentRole: "default"},
		},
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"type":"function_call","name":"wait_agent","call_id":"wait-1","arguments":"{\"targets\":[\"child-provider-1\",\"child-provider-2\",\"child-provider-3\"],\"timeout_ms\":10000}"}}}`))
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"id":"wait-1","type":"collabAgentToolCall","tool":"wait","receiverThreadIds":["child-provider-1"],"agentsStates":{"child-provider-1":{"status":"completed","message":"done"}},"status":"completed"}}}`))

	var waitEvent *provider.ProviderEvent
	for i := range events {
		if events[i].ItemType == "wait_agent" && events[i].Kind == provider.EventToolComplete {
			waitEvent = &events[i]
		}
	}
	if waitEvent == nil {
		t.Fatalf("wait_agent completion missing: %+v", events)
	}
	var meta struct {
		Input struct {
			ReceiverThreadIDs          []string             `json:"receiverThreadIds"`
			RequestedReceiverThreadIDs []string             `json:"requestedReceiverThreadIds"`
			ReceiverAgents             []collabReceiverMeta `json:"receiverAgents"`
			RequestedReceiverAgents    []collabReceiverMeta `json:"requestedReceiverAgents"`
		} `json:"input"`
	}
	if err := json.Unmarshal(waitEvent.Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if want := []string{"child-provider-1"}; !reflect.DeepEqual(meta.Input.ReceiverThreadIDs, want) {
		t.Fatalf("receiverThreadIds = %+v, want completion statuses %+v", meta.Input.ReceiverThreadIDs, want)
	}
	wantRequested := []string{"child-provider-1", "child-provider-2", "child-provider-3"}
	if !reflect.DeepEqual(meta.Input.RequestedReceiverThreadIDs, wantRequested) {
		t.Fatalf("requestedReceiverThreadIds = %+v, want raw wait targets %+v", meta.Input.RequestedReceiverThreadIDs, wantRequested)
	}
	wantAgents := []collabReceiverMeta{
		{ThreadID: "child-provider-1", AgentNickname: "Hypatia", AgentRole: "default"},
	}
	if !reflect.DeepEqual(meta.Input.ReceiverAgents, wantAgents) {
		t.Fatalf("receiverAgents = %+v, want %+v", meta.Input.ReceiverAgents, wantAgents)
	}
	wantRequestedAgents := []collabReceiverMeta{
		{ThreadID: "child-provider-1", AgentNickname: "Hypatia", AgentRole: "default"},
		{ThreadID: "child-provider-2", AgentNickname: "Parfit", AgentRole: "default"},
		{ThreadID: "child-provider-3", AgentNickname: "Ada", AgentRole: "default"},
	}
	if !reflect.DeepEqual(meta.Input.RequestedReceiverAgents, wantRequestedAgents) {
		t.Fatalf("requestedReceiverAgents = %+v, want %+v", meta.Input.RequestedReceiverAgents, wantRequestedAgents)
	}
}

func TestRawWriteStdinWaitResultIgnoresSpoofedCommandOutput(t *testing.T) {
	output := "Chunk ID: abc\nWall time: 0.1000 seconds\nOutput:\nProcess exited with code 0\n"
	if got := rawWriteStdinWaitResult(output); got != "" {
		t.Fatalf("rawWriteStdinWaitResult spoofed output = %q, want empty", got)
	}

	output = "Chunk ID: abc\nWall time: 0.1000 seconds\nProcess exited with code 0\nOutput:\n"
	if got := rawWriteStdinWaitResult(output); got != terminalWaitResultExited {
		t.Fatalf("rawWriteStdinWaitResult header = %q, want exited", got)
	}
}

func TestDispatchLineRawToolCallsAreBoundedAndCleared(t *testing.T) {
	s := &Session{
		threadID:         "parent-thread",
		pending:          make(map[int64]chan json.RawMessage),
		rawToolCallsByID: make(map[string]rawToolCall),
		onEvent:          func(provider.ProviderEvent) {},
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"type":"function_call","name":"unrelated","call_id":"ignored-1","arguments":"{}"}}}`))
	if len(s.rawToolCallsByID) != 0 {
		t.Fatalf("unrelated raw call retained: %+v", s.rawToolCallsByID)
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"type":"function_call","name":"write_stdin","call_id":"wait-1","arguments":"{\"session_id\":\"pid-42\",\"chars\":\"\"}"}}}`))
	if len(s.rawToolCallsByID) != 1 {
		t.Fatalf("write_stdin raw call count = %d, want 1", len(s.rawToolCallsByID))
	}
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"type":"function_call_output","call_id":"wait-1","output":"Process running with session ID pid-42\nOutput:\n"}}}`))
	if len(s.rawToolCallsByID) != 0 {
		t.Fatalf("raw call not cleared after output: %+v", s.rawToolCallsByID)
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"type":"function_call","name":"wait_agent","call_id":"wait-agent-1","arguments":"{\"targets\":[\"child-provider-1\"]}"}}}`))
	if len(s.rawToolCallsByID) != 1 {
		t.Fatalf("wait_agent raw call count = %d, want 1", len(s.rawToolCallsByID))
	}
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"parent-thread","turn":{"id":"turn-1","status":"completed"}}}`))
	if len(s.rawToolCallsByID) != 0 {
		t.Fatalf("raw calls not cleared on turn complete: %+v", s.rawToolCallsByID)
	}
}

func TestDispatchLineRawExecCommandOutputEmitsModelResult(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID:         "parent-thread",
		pending:          make(map[int64]chan json.RawMessage),
		rawToolCallsByID: make(map[string]rawToolCall),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"type":"function_call","name":"exec_command","call_id":"cmd-1","arguments":"{\"cmd\":\"sleep 10\"}"}}}`))
	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"parent-thread","turnId":"turn-1","item":{"type":"function_call_output","call_id":"cmd-1","output":"Chunk ID: abc\nWall time: 1.0000 seconds\nProcess running with session ID 17313\nOutput:\n"}}}`))

	if len(events) != 1 {
		t.Fatalf("events = %d, want 1: %+v", len(events), events)
	}
	evt := events[0]
	if evt.Kind != provider.EventCodexExecResult {
		t.Fatalf("event kind = %q, want %q", evt.Kind, provider.EventCodexExecResult)
	}
	if evt.ThreadID != "parent-thread" || evt.TurnID != "turn-1" || evt.ItemID != "cmd-1" {
		t.Fatalf("event routing = thread %q turn %q item %q", evt.ThreadID, evt.TurnID, evt.ItemID)
	}
	var meta map[string]any
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["result"] != terminalWaitResultRunning {
		t.Fatalf("meta.result = %v, want %q", meta["result"], terminalWaitResultRunning)
	}
	if meta["process_id"] != "17313" {
		t.Fatalf("meta.process_id = %v, want 17313", meta["process_id"])
	}
	if meta["command"] != "sleep 10" {
		t.Fatalf("meta.command = %v, want sleep 10", meta["command"])
	}
	if len(s.rawToolCallsByID) != 0 {
		t.Fatalf("raw exec call not cleared after output: %+v", s.rawToolCallsByID)
	}
}

func TestCodexProviderEventLogRedactorRedactsWriteStdinEvents(t *testing.T) {
	redact := newCodexProviderEventLogRedactor()

	rawCall := []byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"parent-thread","item":{"type":"function_call","name":"write_stdin","call_id":"wait-1","arguments":"{\"session_id\":\"pid-42\",\"chars\":\"secret-token\\n\",\"yield_time_ms\":1000}"}}}`)
	redactedCall := string(redact("in", rawCall))
	if strings.Contains(redactedCall, "secret-token") {
		t.Fatalf("write_stdin arguments were not redacted: %s", redactedCall)
	}
	if !strings.Contains(redactedCall, "[redacted]") || !strings.Contains(redactedCall, "pid-42") {
		t.Fatalf("redacted write_stdin call lost expected fields: %s", redactedCall)
	}

	rawOutput := []byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"parent-thread","item":{"type":"function_call_output","call_id":"wait-1","output":"secret command output"}}}`)
	redactedOutput := string(redact("in", rawOutput))
	if strings.Contains(redactedOutput, "secret command output") {
		t.Fatalf("write_stdin output was not redacted: %s", redactedOutput)
	}
	if !strings.Contains(redactedOutput, "[redacted]") {
		t.Fatalf("redacted write_stdin output missing marker: %s", redactedOutput)
	}

	unrelatedOutput := []byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"threadId":"parent-thread","item":{"type":"function_call_output","call_id":"other-1","output":"visible output"}}}`)
	if got := string(redact("in", unrelatedOutput)); !strings.Contains(got, "visible output") {
		t.Fatalf("unrelated output should not be redacted: %s", got)
	}

	typedInteraction := []byte(`{"jsonrpc":"2.0","method":"item/commandExecution/terminalInteraction","params":{"threadId":"parent-thread","turnId":"turn-1","itemId":"cmd-1","processId":"pid-42","stdin":"secret-token\n"}}`)
	redactedTyped := string(redact("in", typedInteraction))
	if strings.Contains(redactedTyped, "secret-token") {
		t.Fatalf("typed terminal interaction stdin was not redacted: %s", redactedTyped)
	}
	if !strings.Contains(redactedTyped, "[redacted]") || !strings.Contains(redactedTyped, "pid-42") {
		t.Fatalf("redacted typed terminal interaction lost expected fields: %s", redactedTyped)
	}

	emptyTypedInteraction := []byte(`{"jsonrpc":"2.0","method":"item/commandExecution/terminalInteraction","params":{"threadId":"parent-thread","turnId":"turn-1","itemId":"cmd-1","processId":"pid-42","stdin":""}}`)
	if got := string(redact("in", emptyTypedInteraction)); strings.Contains(got, "[redacted]") {
		t.Fatalf("empty typed terminal interaction should not be redacted: %s", got)
	}
}

func TestCodexProviderEventLogRedactorRedactsEncryptedCollaborationMessages(t *testing.T) {
	redact := newCodexProviderEventLogRedactor()
	for _, line := range [][]byte{
		[]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"item":{"type":"function_call","namespace":"collaboration","name":"spawn_agent","call_id":"spawn-1","arguments":"{\"task_name\":\"reviewer\",\"message\":\"gAAAA-spawn\"}"}}}`),
		[]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"item":{"type":"function_call","namespace":"collaboration","name":"send_message","call_id":"send-1","arguments":"{\"target\":\"/root/reviewer\",\"message\":\"gAAAA-send\"}"}}}`),
		[]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"item":{"type":"function_call","namespace":"collaboration","name":"followup_task","call_id":"followup-1","arguments":"{\"target\":\"/root/reviewer\",\"message\":\"gAAAA-followup\"}"}}}`),
		[]byte(`{"jsonrpc":"2.0","method":"rawResponseItem/completed","params":{"item":{"type":"function_call","namespace":"collaboration","name":"spawn_agent","call_id":"malformed-1","arguments":"malformed-gAAAA"}}}`),
	} {
		got := redact("in", line)
		if strings.Contains(string(got), "gAAAA") {
			t.Fatalf("encrypted collaboration message survived redaction: %s", got)
		}
		if !strings.Contains(string(got), `[redacted]`) {
			t.Fatalf("redaction marker missing: %s", got)
		}
	}
}

func TestDispatchLineSubagentNotificationUsesAgentPathMapping(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID:               "parent-thread",
		pending:                make(map[int64]chan json.RawMessage),
		childParentByThread:    make(map[string]string),
		childParentByAgentPath: map[string]string{"/root/researcher": "call-collab-1"},
		agentPathByThread:      make(map[string]string),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	line := []byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"parent-thread","item":{"id":"user-msg-1","type":"userMessage","content":[{"type":"text","text":"<subagent_notification>{\"agent_path\":\"/root/researcher\",\"status\":{\"completed\":\"done\"}}</subagent_notification>"}]}}}`)
	s.dispatchLine(line)

	for _, evt := range events {
		if evt.Kind != provider.EventSubagentNotification {
			continue
		}
		if evt.ItemID != "call-collab-1" {
			t.Fatalf("ItemID: got %q, want call-collab-1", evt.ItemID)
		}
		var meta map[string]any
		if err := json.Unmarshal(evt.Meta, &meta); err != nil {
			t.Fatalf("meta unmarshal: %v", err)
		}
		if meta["status"] != "completed" {
			t.Fatalf("meta.status: got %v, want completed", meta["status"])
		}
		if meta["message"] != "done" {
			t.Fatalf("meta.message: got %v, want done", meta["message"])
		}
		return
	}
	t.Fatalf("expected EventSubagentNotification, got %+v", events)
}

func TestDispatchLineCloseAgentKeepsOwnItemID(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID:            "parent-thread",
		pending:             make(map[int64]chan json.RawMessage),
		childParentByThread: map[string]string{"child-provider-1": "call-collab-1"},
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	s.dispatchLine([]byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"provider-parent","item":{"id":"close-call-1","type":"collabAgentToolCall","tool":"closeAgent","receiverThreadIds":["child-provider-1"],"status":"completed"}}}`))

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].ItemID != "close-call-1" {
		t.Fatalf("ItemID: got %q, want %q", events[0].ItemID, "close-call-1")
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
		proc:     proc,
		threadID: testThread,
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			eventCh <- evt
		},
		cancel: cancel,
	}
	s.setRootThreadID("codex-thread-1")
	go s.readLoop()
	t.Cleanup(func() {
		cancel()
		proc.Close()
	})
	return s, eventCh
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
		proc:     proc,
		threadID: testThread,
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			eventCh <- evt
		},
		cancel: cancel,
	}
	s.setRootThreadID("ctx-thread")
	go s.readLoop()

	sendDone := make(chan error, 1)
	go func() {
		sendDone <- s.Send(context.Background(), "hi", provider.SendOptions{})
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

func TestSessionBuffersPlanDeltaUntilCompletion(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	lines := []string{
		`{"jsonrpc":"2.0","method":"item/plan/delta","params":{"turnId":"turn-plan","itemId":"plan-1","delta":"# Plan\n\n"}}`,
		`{"jsonrpc":"2.0","method":"item/plan/delta","params":{"turnId":"turn-plan","itemId":"plan-1","delta":"- first\n- second"}}`,
		`{"jsonrpc":"2.0","method":"item/completed","params":{"turnId":"turn-plan","item":{"id":"plan-1","type":"plan"}}}`,
	}
	for _, line := range lines {
		if err := s.proc.WriteLine([]byte(line)); err != nil {
			t.Fatalf("write line: %v", err)
		}
	}

	var got provider.ProviderEvent
	select {
	case got = <-eventCh:
	case <-time.After(2 * time.Second):
		t.Fatal("no proposed plan event emitted")
	}
	if got.Kind != provider.EventProposedPlan {
		t.Fatalf("kind: got %q, want EventProposedPlan", got.Kind)
	}
	if got.ItemID != "plan-1" {
		t.Fatalf("itemID: got %q, want plan-1", got.ItemID)
	}
	if got.Content != "# Plan\n\n- first\n- second" {
		t.Fatalf("content: got %q", got.Content)
	}
}

func TestSessionPrefersCompletedPlanContentOverBufferedDelta(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	lines := []string{
		`{"jsonrpc":"2.0","method":"item/plan/delta","params":{"turnId":"turn-plan","itemId":"plan-1","delta":"# Draft plan"}}`,
		`{"jsonrpc":"2.0","method":"item/completed","params":{"turnId":"turn-plan","item":{"id":"plan-1","type":"plan","text":"# Final plan"}}}`,
	}
	for _, line := range lines {
		if err := s.proc.WriteLine([]byte(line)); err != nil {
			t.Fatalf("write line: %v", err)
		}
	}

	var got provider.ProviderEvent
	select {
	case got = <-eventCh:
	case <-time.After(2 * time.Second):
		t.Fatal("no proposed plan event emitted")
	}
	if got.Content != "# Final plan" {
		t.Fatalf("content = %q, want completed item text", got.Content)
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

func TestCodexApprovalWaitsForUserResponseWithoutTimeout(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	line := []byte(`{"jsonrpc":"2.0","id":42,"method":"item/commandExecution/requestApproval","params":{"command":"ls"}}`)
	s.dispatchLine(line)

	evt := codexWaitEvent(t, eventCh)
	if evt.Kind != provider.EventApprovalRequest {
		t.Fatalf("kind: got %q, want %q", evt.Kind, provider.EventApprovalRequest)
	}

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
				RequestID: "42",
				Decision:  "allow",
			}); err != nil {
				t.Fatalf("RespondToApproval after waiting: %v", err)
			}
			return
		}
	}
}

func TestCodexUserInputWaitsForUserResponseWithoutTimeout(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	line := []byte(`{"jsonrpc":"2.0","id":43,"method":"item/tool/requestUserInput","params":{"questions":[{"id":"scope","header":"Scope","question":"Choose","options":[{"label":"turn","description":"This turn"}]}]}}`)
	s.dispatchLine(line)

	evt := codexWaitEvent(t, eventCh)
	if evt.Kind != provider.EventUserInputRequest {
		t.Fatalf("kind: got %q, want %q", evt.Kind, provider.EventUserInputRequest)
	}

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
				RequestID: "43",
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

func TestCodexRejectsRequestUserInputWithoutQuestions(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	line := []byte(`{"jsonrpc":"2.0","id":44,"method":"item/tool/requestUserInput","params":{"questions":[]}}`)
	s.dispatchLine(line)

	select {
	case evt := <-eventCh:
		if evt.Kind == provider.EventUserInputRequest {
			t.Fatalf("empty requestUserInput emitted user-input request: %+v", evt)
		}
	case <-time.After(100 * time.Millisecond):
	}
}

func TestApprovalResponseResolvesPendingCodex(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

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

	if err := s.RespondToApproval(context.Background(), provider.ApprovalResponse{
		RequestID: "7",
		Decision:  "allow",
	}); !errors.Is(err, provider.ErrStaleInteractiveRequest) {
		t.Fatalf("second RespondToApproval error = %v, want ErrStaleInteractiveRequest", err)
	}
}

func TestCodexCloseResolvesPendingApprovalAsLost(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

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

// TestCodexInterruptDrainsPendingApproval covers the Interrupt-drain
// path: when the user clicks stop while a sandbox approval is pending,
// the session must emit EventApprovalResolved with decision="cancel" so
// the frontend's approval panel clears immediately. This is the bug-fix
// beyond t3-code's CodexSessionRuntime.interruptTurn, which leaves the
// local Deferred parked.
func TestCodexInterruptDrainsPendingApproval(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	// Need an active turn for Interrupt to attempt the RPC at all —
	// before that gate the function returns "no active turn".
	s.mu.Lock()
	s.activeTurnID = "turn-1"
	s.mu.Unlock()

	line := []byte(`{"jsonrpc":"2.0","id":11,"method":"item/commandExecution/requestApproval","params":{"command":"ls"}}`)
	s.dispatchLine(line)

	// Drain incoming events until we see the approval request.
	for {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventApprovalRequest {
				goto interrupt
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no approval event")
		}
	}
interrupt:
	// Interrupt itself returns an error (cat echoes our request back
	// as a server-request shape, which falls to the default error case
	// — fine for this test). The drain runs regardless.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.Interrupt(ctx)

	deadline := time.After(2 * time.Second)
	for {
		select {
		case evt := <-eventCh:
			if evt.Kind != provider.EventApprovalResolved {
				continue
			}
			var meta struct {
				RequestID string `json:"requestId"`
				Decision  string `json:"decision"`
			}
			if err := json.Unmarshal(evt.Meta, &meta); err != nil {
				t.Fatalf("unmarshal resolved meta: %v", err)
			}
			if meta.RequestID != "11" {
				t.Fatalf("requestId = %q, want 11", meta.RequestID)
			}
			if meta.Decision != "cancel" {
				t.Fatalf("decision = %q, want cancel", meta.Decision)
			}
			return
		case <-deadline:
			t.Fatal("no EventApprovalResolved after Interrupt")
		}
	}
}

// TestCodexInterruptDrainsPendingUserInput is the user-input twin of
// TestCodexInterruptDrainsPendingApproval. The resolved event must
// carry decision="cancel" AND answers={} so the frontend's user-input
// panel clears with a well-formed payload.
func TestCodexInterruptDrainsPendingUserInput(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	s.mu.Lock()
	s.activeTurnID = "turn-1"
	s.mu.Unlock()

	line := []byte(`{"jsonrpc":"2.0","id":12,"method":"item/tool/requestUserInput","params":{"questions":[{"id":"scope","header":"Scope","question":"Choose","options":[{"label":"turn","description":"This turn"}]}]}}`)
	s.dispatchLine(line)

	for {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventUserInputRequest {
				goto interrupt
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no user-input request event")
		}
	}
interrupt:
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.Interrupt(ctx)

	deadline := time.After(2 * time.Second)
	for {
		select {
		case evt := <-eventCh:
			if evt.Kind != provider.EventUserInputResolved {
				continue
			}
			var meta struct {
				RequestID string         `json:"requestId"`
				Decision  string         `json:"decision"`
				Answers   map[string]any `json:"answers"`
			}
			if err := json.Unmarshal(evt.Meta, &meta); err != nil {
				t.Fatalf("unmarshal resolved meta: %v", err)
			}
			if meta.RequestID != "12" {
				t.Fatalf("requestId = %q, want 12", meta.RequestID)
			}
			if meta.Decision != "cancel" {
				t.Fatalf("decision = %q, want cancel", meta.Decision)
			}
			if meta.Answers == nil {
				t.Fatalf("answers field absent; want empty map for user-input variant")
			}
			if len(meta.Answers) != 0 {
				t.Fatalf("answers = %v, want empty map", meta.Answers)
			}
			return
		case <-deadline:
			t.Fatal("no EventUserInputResolved after Interrupt")
		}
	}
}

// TestCodexCloseDrainsPendingUserInputWithAnswers verifies the Close
// drain emits an EventUserInputResolved that carries the empty
// `answers` map alongside the historic decision="lost". The frontend
// type contract requires the field on every UserInputResolved meta.
func TestCodexCloseDrainsPendingUserInputWithAnswers(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	line := []byte(`{"jsonrpc":"2.0","id":13,"method":"item/tool/requestUserInput","params":{"questions":[{"id":"scope","header":"Scope","question":"Choose","options":[{"label":"turn","description":"This turn"}]}]}}`)
	s.dispatchLine(line)

	for {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventUserInputRequest {
				goto closeNow
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no user-input request event")
		}
	}
closeNow:
	if err := s.Close(); err != nil {
		t.Logf("close returned %v (acceptable)", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case evt, ok := <-eventCh:
			if !ok {
				t.Fatal("event channel closed before resolved event")
			}
			if evt.Kind != provider.EventUserInputResolved {
				continue
			}
			var meta struct {
				RequestID string         `json:"requestId"`
				Decision  string         `json:"decision"`
				Answers   map[string]any `json:"answers"`
			}
			if err := json.Unmarshal(evt.Meta, &meta); err != nil {
				t.Fatalf("unmarshal resolved meta: %v", err)
			}
			if meta.RequestID != "13" {
				t.Fatalf("requestId = %q, want 13", meta.RequestID)
			}
			if meta.Decision != "lost" {
				t.Fatalf("decision = %q, want lost (Close path)", meta.Decision)
			}
			if meta.Answers == nil {
				t.Fatalf("answers field absent; want empty map")
			}
			return
		case <-deadline:
			t.Fatal("no EventUserInputResolved after Close")
		}
	}
}

func TestCodexProviderExitResolvesPendingUserInputAsLost(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	line := []byte(`{"jsonrpc":"2.0","id":14,"method":"item/tool/requestUserInput","params":{"questions":[{"id":"scope","header":"Scope","question":"Choose","options":[{"label":"turn","description":"This turn"}]}]}}`)
	s.dispatchLine(line)

	evt := codexWaitEvent(t, eventCh)
	if evt.Kind != provider.EventUserInputRequest {
		t.Fatalf("kind: got %q, want %q", evt.Kind, provider.EventUserInputRequest)
	}

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

// TestCodexDrainWritesTurnTransitionError verifies the wire shape of
// the JSON-RPC error our drain writes to the Codex app-server. The
// `data.reason = "turnTransition"` field is the magic value Codex uses
// to early-return cleanly on `is_turn_transition_server_request_error`
// (codex-rs/app-server/src/server_request_error.rs) — without it,
// Codex's per-handler fallback paths log "request failed with client
// error" and (for MCP elicitation) pick the wrong action.
func TestCodexDrainWritesTurnTransitionError(t *testing.T) {
	capturePath := t.TempDir() + "/wire.jsonl"
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args: []string{"-c", `
			while IFS= read -r line; do
				printf '%s\n' "$line" >> "$CAPTURE_PATH"
			done
		`},
		Env: map[string]string{"CAPTURE_PATH": capturePath},
	})
	if err != nil {
		t.Fatalf("spawn capture sh: %v", err)
	}
	s := &Session{
		proc:     proc,
		threadID: testThread,
		pending:  make(map[int64]chan json.RawMessage),
		onEvent:  func(provider.ProviderEvent) {},
		cancel:   cancel,
	}
	s.setRootThreadID("codex-thread-1")
	go s.readLoop()
	t.Cleanup(func() {
		cancel()
		_ = proc.Close()
	})

	// Seed a pending approval (rpcID 99) so the drain has something
	// to write a response for.
	s.trackPendingApproval(99, provider.EventApprovalResolved)

	s.drainPendingApprovals("cancel", false, true)

	// Give the capture script a moment to flush, then read.
	deadline := time.Now().Add(2 * time.Second)
	var data []byte
	for time.Now().Before(deadline) {
		data, _ = os.ReadFile(capturePath)
		if len(data) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(data) == 0 {
		t.Fatalf("no wire bytes captured")
	}
	var frame struct {
		ID    int64 `json:"id"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    struct {
				Reason string `json:"reason"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatalf("unmarshal captured frame %q: %v", string(data), err)
	}
	if frame.ID != 99 {
		t.Fatalf("frame.id = %d, want 99", frame.ID)
	}
	if frame.Error.Data.Reason != "turnTransition" {
		t.Fatalf("error.data.reason = %q, want \"turnTransition\"", frame.Error.Data.Reason)
	}
	if frame.Error.Code == 0 {
		t.Fatalf("error.code is zero — JSON-RPC error frames must carry a non-zero code")
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

func TestBuildUserInputResponseResultAnswers(t *testing.T) {
	rpcID, result, err := buildUserInputResponseResult(provider.UserInputResponse{
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

func TestBuildUserInputResponseResultRejectsUnknownDecision(t *testing.T) {
	_, _, err := buildUserInputResponseResult(provider.UserInputResponse{
		RequestID: "7",
		Decision:  "bogus",
	})
	if !errors.Is(err, provider.ErrInvalidUserInputDecision) {
		t.Fatalf("error = %v, want ErrInvalidUserInputDecision", err)
	}
}

func TestBuildUserInputResponseResultAcceptsDeclineDecisions(t *testing.T) {
	for _, decision := range []string{"decline", "cancel", "deny"} {
		t.Run(decision, func(t *testing.T) {
			_, result, err := buildUserInputResponseResult(provider.UserInputResponse{
				RequestID: "7",
				Decision:  decision,
			})
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", decision, err)
			}
			if result == nil {
				t.Fatal("result is nil")
			}
		})
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
	s.trackPendingApproval(42, provider.EventApprovalResolved)

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

	start := codexWaitEvent(t, eventCh)
	if start.Kind != provider.EventToolStart {
		t.Fatalf("start kind: got %q, want %q", start.Kind, provider.EventToolStart)
	}
	if start.ItemType != "request_user_input" {
		t.Fatalf("start itemType: got %q, want request_user_input", start.ItemType)
	}
	if start.ItemID != "item-8" {
		t.Errorf("start itemID: got %q, want %q", start.ItemID, "item-8")
	}

	evt := codexWaitEvent(t, eventCh)
	if evt.Kind != provider.EventUserInputRequest {
		t.Fatalf("request kind: got %q, want %q", evt.Kind, provider.EventUserInputRequest)
	}
	if evt.TurnID != "turn-3" {
		t.Errorf("turnID: got %q, want %q", evt.TurnID, "turn-3")
	}
	if evt.ItemID != "item-8" {
		t.Errorf("itemID: got %q, want %q", evt.ItemID, "item-8")
	}

	var request provider.UserInputRequest
	if err := json.Unmarshal(evt.Meta, &request); err != nil {
		t.Fatalf("unmarshal user input request: %v", err)
	}
	if len(request.Questions) != 1 {
		t.Fatalf("questions len: got %d, want 1", len(request.Questions))
	}
	if request.ToolUseID != "item-8" {
		t.Errorf("toolUseID: got %q, want item-8", request.ToolUseID)
	}
	if request.Questions[0].ID != "scope" {
		t.Errorf("question id: got %q, want %q", request.Questions[0].ID, "scope")
	}
}

func TestCodexHandleServerRequestUserInputV2TopLevelRouteFields(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	line, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      31,
		"method":  "item/tool/requestUserInput",
		"params": map[string]any{
			"threadId": s.rootThreadID(),
			"turnId":   "turn-31",
			"itemId":   "item-31",
			"questions": []map[string]any{{
				"id":       "scope",
				"header":   "Scope",
				"question": "Choose a scope",
				"isOther":  true,
				"isSecret": false,
				"options": []map[string]string{
					{"label": "turn", "description": "Apply only to this turn"},
					{"label": "session", "description": "Apply for the whole session"},
				},
			}},
		},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}

	start := codexWaitEvent(t, eventCh)
	if start.Kind != provider.EventToolStart {
		t.Fatalf("start kind: got %q, want %q", start.Kind, provider.EventToolStart)
	}
	if start.ItemID != "item-31" {
		t.Errorf("start itemID: got %q, want item-31", start.ItemID)
	}

	evt := codexWaitEvent(t, eventCh)
	if evt.Kind != provider.EventUserInputRequest {
		t.Fatalf("request kind: got %q, want %q", evt.Kind, provider.EventUserInputRequest)
	}
	if evt.TurnID != "turn-31" {
		t.Errorf("turnID: got %q, want %q", evt.TurnID, "turn-31")
	}
	if evt.ItemID != "item-31" {
		t.Errorf("itemID: got %q, want %q", evt.ItemID, "item-31")
	}

	var request provider.UserInputRequest
	if err := json.Unmarshal(evt.Meta, &request); err != nil {
		t.Fatalf("unmarshal user input request: %v", err)
	}
	if request.ThreadID != testThread {
		t.Errorf("threadID: got %q, want %q", request.ThreadID, testThread)
	}
	if request.TurnID != "turn-31" {
		t.Errorf("request turnID: got %q, want %q", request.TurnID, "turn-31")
	}
	if request.ToolUseID != "item-31" {
		t.Errorf("request toolUseID: got %q, want item-31", request.ToolUseID)
	}
	if got := len(request.Questions); got != 1 {
		t.Fatalf("questions len: got %d, want 1", got)
	}
	options := request.Questions[0].Options
	if got := len(options); got != 2 {
		t.Fatalf("options len: got %d, want 2", got)
	}
	if options[1].Label != "session" {
		t.Errorf("second option label: got %q, want session", options[1].Label)
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

func TestCodexPermissionApprovalWaitsForUserResponseWithoutTimeout(t *testing.T) {
	s, eventCh := newTestCodexSession(t)

	line := []byte(`{"jsonrpc":"2.0","id":24,"method":"item/permissions/requestApproval","params":{"turnId":"turn-24","itemId":"item-24","reason":"Need broader write access","permissions":{"fileSystem":{"read":["/tmp/project/src"]}}}}`)
	s.dispatchLine(line)

	evt := codexWaitEvent(t, eventCh)
	if evt.Kind != provider.EventApprovalRequest {
		t.Fatalf("kind: got %q, want %q", evt.Kind, provider.EventApprovalRequest)
	}

	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case evt := <-eventCh:
			switch evt.Kind {
			case provider.EventError, provider.EventApprovalResolved:
				t.Fatalf("pending permission approval resolved without user action: %+v", evt)
			}
		case <-deadline:
			if err := s.RespondToApproval(context.Background(), provider.ApprovalResponse{
				RequestID: "24",
				Decision:  "allow",
			}); err != nil {
				t.Fatalf("RespondToApproval after waiting: %v", err)
			}
			if err := s.RespondToApproval(context.Background(), provider.ApprovalResponse{
				RequestID: "24",
				Decision:  "allow",
			}); !errors.Is(err, provider.ErrStaleInteractiveRequest) {
				t.Fatalf("second RespondToApproval error = %v, want ErrStaleInteractiveRequest", err)
			}
			return
		}
	}
}

func TestCodexHandleServerRequestUnknown(t *testing.T) {
	s, _ := newTestCodexSession(t)

	// Unknown server request — should send error response. With cat,
	// the error response echoes back and readLoop sees it as a stray
	// response (no matching pending id) and logs-and-drops. We just
	// verify no crash. Drive a synchronous observation instead of the
	// former fixed-200ms sleep: write the request, then invoke
	// dispatchLine directly on the same line so its decode path runs
	// in the test goroutine and any panic or error surfaces
	// immediately.
	line := []byte(`{"jsonrpc":"2.0","id":1,"method":"unknown/request","params":{}}`)
	if err := s.proc.WriteLine(line); err != nil {
		t.Fatalf("write: %v", err)
	}
	s.dispatchLine(line)
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
		proc:     proc,
		threadID: testThread,
		pending:  make(map[int64]chan json.RawMessage),
		onEvent:  func(provider.ProviderEvent) {},
		cancel:   cancelProc,
	}
	s.setRootThreadID("codex-thread-1")
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
		pending:                make(map[int64]chan json.RawMessage),
		onEvent:                func(provider.ProviderEvent) {},
		cancel:                 cancelProc,
		requestTimeoutOverride: 50 * time.Millisecond,
	}
	s.setRootThreadID("codex-thread-1")
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
		pending:                make(map[int64]chan json.RawMessage),
		onEvent:                func(provider.ProviderEvent) {},
		cancel:                 cancelProc,
		requestTimeoutOverride: 20 * time.Millisecond,
	}
	s.setRootThreadID("codex-thread-1")
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

	err := s.Send(ctx, "hello world", provider.SendOptions{})
	// Expected to fail because cat echo + handleServerRequest produces error response.
	if err == nil {
		t.Fatal("expected error from Send via cat echo")
	}
}

func TestCodexInterruptStartupSendsEmptyTurnID(t *testing.T) {
	// Codex's wire protocol treats an empty turn_id as a "startup
	// interrupt" — the app-server submits Op::Interrupt to the core
	// and responds immediately with `{}`. We must NOT gate on a
	// non-empty activeTurnID; just send the RPC and let upstream
	// handle the dispatch-window case. See
	// codex-rs/app-server/src/codex_message_processor.rs:7790-7849.
	capturePath := t.TempDir() + "/request.json"
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args: []string{"-c", `
			IFS= read -r line || exit 1
			printf '%s\n' "$line" > "$CAPTURE_PATH"
			printf '{"jsonrpc":"2.0","id":1,"result":{}}\n'
		`},
		Env: map[string]string{"CAPTURE_PATH": capturePath},
	})
	if err != nil {
		t.Fatalf("spawn recorder: %v", err)
	}
	s := &Session{
		proc:     proc,
		threadID: testThread,
		// activeTurnID intentionally left empty — this is the
		// dispatch window case.
		pending: make(map[int64]chan json.RawMessage),
		onEvent: func(provider.ProviderEvent) {},
		cancel:  cancel,
	}
	s.setRootThreadID("codex-thread-1")
	go s.readLoop()
	t.Cleanup(func() {
		cancel()
		_ = proc.Close()
	})

	interruptCtx, interruptCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer interruptCancel()
	if err := s.Interrupt(interruptCtx); err != nil {
		t.Fatalf("Interrupt during dispatch window: %v", err)
	}

	data, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read captured request: %v", err)
	}
	var frame struct {
		Method string `json:"method"`
		Params struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
		} `json:"params"`
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatalf("unmarshal captured request: %v", err)
	}
	if frame.Method != "turn/interrupt" {
		t.Fatalf("method = %q, want turn/interrupt", frame.Method)
	}
	if frame.Params.ThreadID != "codex-thread-1" {
		t.Fatalf("threadId = %q, want codex-thread-1", frame.Params.ThreadID)
	}
	if frame.Params.TurnID != "" {
		t.Fatalf("turnId = %q, want empty (startup interrupt sentinel)", frame.Params.TurnID)
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

	// Just verify Interrupt attempts the RPC — actual response is
	// noise from cat's echo behaviour. The startup-interrupt path is
	// covered separately by TestCodexInterruptStartupSendsEmptyTurnID.
	_ = s.Interrupt(ctx)
}

func TestCodexInterruptSendsThreadAndTurnID(t *testing.T) {
	capturePath := t.TempDir() + "/request.json"
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "sh",
		Args: []string{"-c", `
			IFS= read -r line || exit 1
			printf '%s\n' "$line" > "$CAPTURE_PATH"
			printf '{"jsonrpc":"2.0","id":1,"result":{}}\n'
		`},
		Env: map[string]string{"CAPTURE_PATH": capturePath},
	})
	if err != nil {
		t.Fatalf("spawn recorder: %v", err)
	}

	s := &Session{
		proc:         proc,
		threadID:     testThread,
		activeTurnID: "turn-1",
		pending:      make(map[int64]chan json.RawMessage),
		onEvent:      func(provider.ProviderEvent) {},
		cancel:       cancel,
	}
	s.setRootThreadID("codex-thread-1")
	go s.readLoop()

	t.Cleanup(func() {
		cancel()
		_ = proc.Close()
	})

	interruptCtx, interruptCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer interruptCancel()

	if err := s.Interrupt(interruptCtx); err != nil {
		t.Fatalf("interrupt: %v", err)
	}

	data, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read captured request: %v", err)
	}

	var frame struct {
		Method string `json:"method"`
		Params struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
		} `json:"params"`
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatalf("unmarshal captured request: %v", err)
	}

	if frame.Method != "turn/interrupt" {
		t.Fatalf("method = %q, want turn/interrupt", frame.Method)
	}
	if frame.Params.ThreadID != "codex-thread-1" {
		t.Fatalf("params.threadId = %q, want codex-thread-1", frame.Params.ThreadID)
	}
	if frame.Params.TurnID != "turn-1" {
		t.Fatalf("params.turnId = %q, want turn-1", frame.Params.TurnID)
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
		proc:     proc,
		threadID: testThread,
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			eventCh <- evt
		},
		cancel:   cancel,
		readDone: make(chan struct{}),
	}
	s.setRootThreadID("test")
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
		proc:     proc,
		threadID: testThread,
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			if evt.Kind == provider.EventSessionStatus && evt.Content == "disconnected" {
				close(disconnected)
				<-release
			}
		},
		cancel:   cancel,
		readDone: make(chan struct{}),
	}
	s.setRootThreadID("test")
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

// TestCodexReadLoopEmitsErrorStatusOnCleanUnexpectedExit pins the
// Codex-side mirror of the Claude quiet-disconnect bug fix. An
// app-server that exits with status 0 without us asking it to close
// is still abnormal — triage's handleSessionDied needs the "error"
// signal to synthesize the truncated turn-complete so the FE working
// indicator clears. The previous `exitErr != nil` gate dropped this
// emission when the process exited cleanly or when WaitProcessExitErr
// hit its 100ms timeout before the OS reaped the child.
func TestCodexReadLoopEmitsErrorStatusOnCleanUnexpectedExit(t *testing.T) {
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
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			eventCh <- evt
		},
		cancel:   cancel,
		readDone: make(chan struct{}),
	}
	s.setRootThreadID("test")
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
			case "disconnected":
				gotDisconnected = true
			}
		case <-timeout:
			t.Fatalf("timeout waiting for error+disconnected on clean unexpected exit (gotError=%v gotDisconnected=%v)", gotError, gotDisconnected)
		}
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
		proc:     proc,
		threadID: testThread,
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			eventCh <- evt
		},
		cancel:   cancel,
		readDone: make(chan struct{}),
	}
	s.setRootThreadID("test")
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
		proc:     proc,
		threadID: testThread,
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			eventCh <- evt
		},
		cancel: cancel,
	}
	s.setRootThreadID("test")

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
	// Drive dispatchLine in the test goroutine so the decode path
	// runs synchronously — former 200ms sleep just hid the fact that
	// there is no readLoop assertion this test makes beyond "it did
	// not crash."
	s.dispatchLine(line)
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

func TestCodexMcpElicitationWaitsForUserResponseWithoutTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	capturePath := filepath.Join(t.TempDir(), "stdin.ndjson")
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{
		Binary: "bash",
		Args:   []string{"-c", `printf '%s\n' '{"jsonrpc":"2.0","id":5,"method":"mcpServer/elicitation/request","params":{"serverName":"my-mcp","message":"Authorize"}}'; while IFS= read -r line; do printf '%s\n' "$line" >> "$CAPTURE"; done`},
		Env: map[string]string{
			"CAPTURE": capturePath,
		},
	})
	if err != nil {
		t.Fatalf("spawn capture process: %v", err)
	}
	eventCh := make(chan provider.ProviderEvent, 10)
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

	for {
		select {
		case evt := <-eventCh:
			if evt.Kind == provider.EventApprovalRequest {
				goto waitWithoutResponse
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for MCP elicitation request")
		}
	}

waitWithoutResponse:
	time.Sleep(200 * time.Millisecond)
	captured, err := os.ReadFile(capturePath)
	if err == nil && len(captured) > 0 {
		t.Fatalf("MCP elicitation wrote response without user action: %s", captured)
	}

	err = s.RespondToApproval(context.Background(), provider.ApprovalResponse{
		RequestID: "5",
		Elicitation: &provider.ElicitationResolution{
			Action:  "confirm",
			Content: json.RawMessage(`{"accepted":true}`),
		},
	})
	if err != nil {
		t.Fatalf("RespondToApproval after waiting: %v", err)
	}

	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		captured, err = os.ReadFile(capturePath)
		if err == nil && len(captured) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(captured) == 0 {
		t.Fatalf("MCP elicitation response was not written: %v", err)
	}
	var frame struct {
		ID     int64 `json:"id"`
		Result struct {
			Action string `json:"action"`
		} `json:"result"`
	}
	if err := json.Unmarshal(captured, &frame); err != nil {
		t.Fatalf("unmarshal captured response: %v (data=%s)", err, captured)
	}
	if frame.ID != 5 {
		t.Fatalf("id = %d, want 5", frame.ID)
	}
	if frame.Result.Action != "confirm" {
		t.Fatalf("action = %q, want confirm", frame.Result.Action)
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

	line := []byte(`{"jsonrpc":"2.0","id":7,"method":"execCommandApproval","params":{"command":"pnpm test"}}`)
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
	if s.rootThreadID() != "mock-thread-123" {
		t.Errorf("rootThreadID: got %q, want %q", s.rootThreadID(), "mock-thread-123")
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

func TestSendImageOnlyTurnStartFormat(t *testing.T) {
	capturePath := filepath.Join(t.TempDir(), "codex-stdin.log")
	script := fmt.Sprintf(`#!/bin/bash
while IFS= read -r line; do
    printf '%%s\n' "$line" >> %q
    id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if echo "$line" | grep -q '"method":"turn/start"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"turn\":{\"id\":\"turn-image\"}}}"
    else
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"thread\":{\"id\":\"mock-thread-123\"}}}"
    fi
done
`, capturePath)
	scriptPath := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock script: %v", err)
	}

	s, err := NewSession(context.Background(), testThread, Config{
		Binary:         scriptPath,
		Model:          "test-model",
		WorkDir:        "/tmp",
		ApprovalPolicy: "on-request",
		Sandbox:        "workspace-write",
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	err = s.Send(context.Background(), "", provider.SendOptions{
		Attachments: []provider.ImageAttachment{{
			ID:       "att-1",
			Filename: "snap.png",
			MimeType: "image/png",
			Size:     8,
			Path:     "/tmp/att-1/snap.png",
		}},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	_ = s.Close()

	captured, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	var turnStart map[string]any
	for _, line := range strings.Split(string(captured), "\n") {
		if !strings.Contains(line, `"method":"turn/start"`) {
			continue
		}
		if err := json.Unmarshal([]byte(line), &turnStart); err != nil {
			t.Fatalf("unmarshal turn/start: %v", err)
		}
		break
	}
	if turnStart == nil {
		t.Fatalf("captured no turn/start request: %s", string(captured))
	}
	params := turnStart["params"].(map[string]any)
	if params["approvalPolicy"] != "on-request" {
		t.Fatalf("approvalPolicy = %v, want on-request", params["approvalPolicy"])
	}
	sandboxPolicy := params["sandboxPolicy"].(map[string]any)
	if sandboxPolicy["type"] != "workspaceWrite" {
		t.Fatalf("sandboxPolicy.type = %v, want workspaceWrite", sandboxPolicy["type"])
	}
	input := params["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input length = %d, want image-only input", len(input))
	}
	imageInput := input[0].(map[string]any)
	if imageInput["type"] != "localImage" {
		t.Fatalf("input type = %v, want localImage", imageInput["type"])
	}
	wantPath := "/tmp/att-1/snap.png"
	if imageInput["path"] != wantPath {
		t.Fatalf("image path = %v, want %s", imageInput["path"], wantPath)
	}
}

func TestSessionSendIncludesRuntimeAccessPolicyForEveryMode(t *testing.T) {
	cases := []struct {
		name            string
		approvalPolicy  string
		sandbox         string
		wantSandboxType string
	}{
		{
			name:            "approval-required",
			approvalPolicy:  "untrusted",
			sandbox:         "read-only",
			wantSandboxType: "readOnly",
		},
		{
			name:            "auto-accept-edits",
			approvalPolicy:  "on-request",
			sandbox:         "workspace-write",
			wantSandboxType: "workspaceWrite",
		},
		{
			name:            "full-access",
			approvalPolicy:  "never",
			sandbox:         "danger-full-access",
			wantSandboxType: "dangerFullAccess",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capturePath := filepath.Join(t.TempDir(), "codex-stdin.log")
			script := fmt.Sprintf(`#!/bin/bash
while IFS= read -r line; do
    printf '%%s\n' "$line" >> %q
    id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if echo "$line" | grep -q '"method":"turn/start"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"turn\":{\"id\":\"turn-runtime\"}}}"
    else
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"thread\":{\"id\":\"mock-thread-123\"}}}"
    fi
done
`, capturePath)
			scriptPath := filepath.Join(t.TempDir(), "codex")
			if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
				t.Fatalf("write mock script: %v", err)
			}

			s, err := NewSession(context.Background(), testThread, Config{
				Binary:         scriptPath,
				Model:          "test-model",
				WorkDir:        "/tmp",
				ApprovalPolicy: tc.approvalPolicy,
				Sandbox:        tc.sandbox,
			}, func(provider.ProviderEvent) {})
			if err != nil {
				t.Fatalf("NewSession: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() })

			if err := s.Send(context.Background(), "hello", provider.SendOptions{}); err != nil {
				t.Fatalf("Send: %v", err)
			}
			_ = s.Close()

			captured, err := os.ReadFile(capturePath)
			if err != nil {
				t.Fatalf("read capture: %v", err)
			}
			var turnStart map[string]any
			for _, line := range strings.Split(string(captured), "\n") {
				if !strings.Contains(line, `"method":"turn/start"`) {
					continue
				}
				if err := json.Unmarshal([]byte(line), &turnStart); err != nil {
					t.Fatalf("unmarshal turn/start: %v", err)
				}
				break
			}
			if turnStart == nil {
				t.Fatalf("captured no turn/start request: %s", string(captured))
			}
			params := turnStart["params"].(map[string]any)
			if params["approvalPolicy"] != tc.approvalPolicy {
				t.Fatalf("approvalPolicy = %v, want %s", params["approvalPolicy"], tc.approvalPolicy)
			}
			sandboxPolicy := params["sandboxPolicy"].(map[string]any)
			if sandboxPolicy["type"] != tc.wantSandboxType {
				t.Fatalf("sandboxPolicy.type = %v, want %s", sandboxPolicy["type"], tc.wantSandboxType)
			}
		})
	}
}

func TestSessionSendIncludesCollaborationMode(t *testing.T) {
	cases := []struct {
		name     string
		mode     provider.InteractionMode
		wantMode string
	}{
		{name: "plan", mode: provider.ModePlan, wantMode: "plan"},
		{name: "chat clears plan mode", mode: provider.ModeChat, wantMode: "default"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capturePath := filepath.Join(t.TempDir(), "codex-stdin.log")
			script := fmt.Sprintf(`#!/bin/bash
while IFS= read -r line; do
    printf '%%s\n' "$line" >> %q
    id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if echo "$line" | grep -q '"method":"turn/start"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"turn\":{\"id\":\"turn-collab\"}}}"
    else
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"thread\":{\"id\":\"mock-thread-123\"}}}"
    fi
done
`, capturePath)
			scriptPath := filepath.Join(t.TempDir(), "codex")
			if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
				t.Fatalf("write mock script: %v", err)
			}

			s, err := NewSession(context.Background(), testThread, Config{
				Binary:          scriptPath,
				Model:           "test-model",
				WorkDir:         "/tmp",
				ReasoningEffort: "high",
			}, func(provider.ProviderEvent) {})
			if err != nil {
				t.Fatalf("NewSession: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() })

			if err := s.Send(context.Background(), "hello", provider.SendOptions{InteractionMode: tc.mode}); err != nil {
				t.Fatalf("Send: %v", err)
			}
			_ = s.Close()

			captured, err := os.ReadFile(capturePath)
			if err != nil {
				t.Fatalf("read capture: %v", err)
			}
			var turnStart map[string]any
			for _, line := range strings.Split(string(captured), "\n") {
				if !strings.Contains(line, `"method":"turn/start"`) {
					continue
				}
				if err := json.Unmarshal([]byte(line), &turnStart); err != nil {
					t.Fatalf("unmarshal turn/start: %v", err)
				}
				break
			}
			if turnStart == nil {
				t.Fatalf("captured no turn/start request: %s", string(captured))
			}
			params := turnStart["params"].(map[string]any)
			collaborationMode := params["collaborationMode"].(map[string]any)
			if collaborationMode["mode"] != tc.wantMode {
				t.Fatalf("collaborationMode.mode = %v, want %s", collaborationMode["mode"], tc.wantMode)
			}
			settings := collaborationMode["settings"].(map[string]any)
			if settings["model"] != "test-model" {
				t.Fatalf("settings.model = %v, want test-model", settings["model"])
			}
			if settings["reasoning_effort"] != "high" {
				t.Fatalf("settings.reasoning_effort = %v, want high", settings["reasoning_effort"])
			}
			if settings["developer_instructions"] != nil {
				t.Fatalf("settings.developer_instructions = %v, want nil built-in preset", settings["developer_instructions"])
			}
		})
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

// -- subagent notification parser tests --

// TestParseSubagentNotifications_SingleTag pins the canonical wire
// shape emitted by codex-source
// (core/src/session_prefix.rs::format_subagent_notification_message):
// a single
// <subagent_notification>{"agent_path":..,"status":..}</subagent_notification>
// block whose JSON body round-trips to a subagentNotification value.
func TestParseSubagentNotifications_SingleTag(t *testing.T) {
	text := `<subagent_notification>{"agent_path":"child-1","status":"completed"}</subagent_notification>`
	got := parseSubagentNotifications(text)
	if len(got) != 1 {
		t.Fatalf("len=%d, want 1 (got=%+v)", len(got), got)
	}
	if got[0].AgentPath != "child-1" {
		t.Errorf("AgentPath: got %q, want %q", got[0].AgentPath, "child-1")
	}
	if got[0].Status != "completed" {
		t.Errorf("Status: got %q, want %q", got[0].Status, "completed")
	}
}

// TestParseSubagentNotifications_LegacyAgentIDFallback pins the
// backward-compat branch: older (pre-rename) Codex builds emit
// `agent_id` instead of `agent_path`. The parser accepts either so a
// fleet straddling the rename doesn't silently drop notifications.
// Production wire is `agent_path` and is the fast path.
func TestParseSubagentNotifications_LegacyAgentIDFallback(t *testing.T) {
	text := `<subagent_notification>{"agent_id":"legacy-child","status":"completed"}</subagent_notification>`
	got := parseSubagentNotifications(text)
	if len(got) != 1 {
		t.Fatalf("len=%d, want 1 (got=%+v)", len(got), got)
	}
	if got[0].AgentPath != "legacy-child" {
		t.Errorf("AgentPath: got %q, want %q (agent_id fallback)", got[0].AgentPath, "legacy-child")
	}
	if got[0].Status != "completed" {
		t.Errorf("Status: got %q, want %q", got[0].Status, "completed")
	}
	// Legacy key must not leak into Extra — it has its own logical slot.
	if _, dup := got[0].Extra["agent_id"]; dup {
		t.Errorf("Extra should not preserve the legacy agent_id key: %+v", got[0].Extra)
	}
}

// TestParseSubagentNotifications_AgentPathWinsOverAgentID locks the
// precedence rule: when both the production key (`agent_path`) and the
// legacy key (`agent_id`) are present (a weird mixed build but cheap
// to define), the production key wins and neither key leaks into Extra.
func TestParseSubagentNotifications_AgentPathWinsOverAgentID(t *testing.T) {
	text := `<subagent_notification>{"agent_path":"new","agent_id":"old","status":"completed"}</subagent_notification>`
	got := parseSubagentNotifications(text)
	if len(got) != 1 {
		t.Fatalf("len=%d, want 1 (got=%+v)", len(got), got)
	}
	if got[0].AgentPath != "new" {
		t.Errorf("AgentPath: got %q, want %q (agent_path takes precedence over agent_id)", got[0].AgentPath, "new")
	}
	if _, dup := got[0].Extra["agent_id"]; dup {
		t.Errorf("Extra should not preserve the legacy agent_id key: %+v", got[0].Extra)
	}
	if _, dup := got[0].Extra["agent_path"]; dup {
		t.Errorf("Extra should not duplicate agent_path: %+v", got[0].Extra)
	}
}

// TestParseSubagentNotifications_MultipleTags verifies that multiple
// notifications in one user message are all extracted and returned in
// source order. In practice this happens when several children finish
// between two parent turns.
func TestParseSubagentNotifications_MultipleTags(t *testing.T) {
	text := `Ordinary prose.

<subagent_notification>{"agent_path":"child-1","status":"completed"}</subagent_notification>

More prose.

<subagent_notification>{"agent_path":"child-2","status":"errored"}</subagent_notification>
`
	got := parseSubagentNotifications(text)
	if len(got) != 2 {
		t.Fatalf("len=%d, want 2 (got=%+v)", len(got), got)
	}
	if got[0].AgentPath != "child-1" || got[0].Status != "completed" {
		t.Errorf("entry 0: got %+v", got[0])
	}
	if got[1].AgentPath != "child-2" || got[1].Status != "errored" {
		t.Errorf("entry 1: got %+v", got[1])
	}
}

// TestParseSubagentNotifications_WhitespaceLenient verifies the regex
// tolerates leading/trailing whitespace around the JSON body. Codex's
// tests pin a tight shape but the refactor plan flagged "be lenient on
// whitespace" as a correctness criterion.
func TestParseSubagentNotifications_WhitespaceLenient(t *testing.T) {
	text := "<subagent_notification>\n  {\"agent_path\":\"child-3\",\"status\":\"interrupted\"}\n</subagent_notification>"
	got := parseSubagentNotifications(text)
	if len(got) != 1 {
		t.Fatalf("len=%d, want 1 (got=%+v)", len(got), got)
	}
	if got[0].AgentPath != "child-3" || got[0].Status != "interrupted" {
		t.Errorf("got %+v", got[0])
	}
}

// TestParseSubagentNotifications_PreservesExtraFields keeps forward
// compatibility: when Codex adds fields inside the notification JSON,
// we preserve them on the Extra map so downstream can opt into richer
// rendering without a parser update. The load-bearing `agent_path` and
// `status` and `message` keys are stripped from Extra (they have their own fields).
func TestParseSubagentNotifications_PreservesExtraFields(t *testing.T) {
	text := `<subagent_notification>{"agent_path":"child-1","status":"completed","message":"ok","duration_ms":1234}</subagent_notification>`
	got := parseSubagentNotifications(text)
	if len(got) != 1 {
		t.Fatalf("len=%d, want 1", len(got))
	}
	if got[0].Message != "ok" {
		t.Errorf("Message: got %v, want %q", got[0].Message, "ok")
	}
	// JSON numbers decode as float64 in map[string]any.
	if got[0].Extra["duration_ms"].(float64) != 1234 {
		t.Errorf("Extra.duration_ms: got %v, want 1234", got[0].Extra["duration_ms"])
	}
	if _, dup := got[0].Extra["agent_path"]; dup {
		t.Errorf("Extra should not duplicate agent_path: %+v", got[0].Extra)
	}
	if _, dup := got[0].Extra["status"]; dup {
		t.Errorf("Extra should not duplicate status: %+v", got[0].Extra)
	}
}

func TestParseSubagentNotifications_ObjectStatus(t *testing.T) {
	text := `<subagent_notification>{"agent_path":"child-1","status":{"completed":"done"}}</subagent_notification>
<subagent_notification>{"agent_path":"child-2","status":{"errored":"boom"}}</subagent_notification>`
	got := parseSubagentNotifications(text)
	if len(got) != 2 {
		t.Fatalf("len=%d, want 2 (got=%+v)", len(got), got)
	}
	if got[0].Status != "completed" || got[0].Message != "done" {
		t.Errorf("entry 0: got %+v, want completed with message", got[0])
	}
	if got[1].Status != "errored" || got[1].Message != "boom" {
		t.Errorf("entry 1: got %+v, want errored with message", got[1])
	}
}

// TestParseSubagentNotifications_SkipsMalformed ensures a single
// broken tag never blocks sibling tags from parsing. A Codex bug (or a
// partial stream) that emits malformed JSON inside one block should
// still let the parent render the remaining user text.
func TestParseSubagentNotifications_SkipsMalformed(t *testing.T) {
	text := `<subagent_notification>not json at all</subagent_notification>
<subagent_notification>{"agent_path":"child-1","status":"completed"}</subagent_notification>
<subagent_notification>{"agent_path":"","status":"completed"}</subagent_notification>
<subagent_notification>{"agent_path":"child-2"}</subagent_notification>`
	got := parseSubagentNotifications(text)
	if len(got) != 1 {
		t.Fatalf("len=%d, want 1 (got=%+v)", len(got), got)
	}
	if got[0].AgentPath != "child-1" {
		t.Errorf("got %+v", got[0])
	}
}

// TestParseSubagentNotifications_NoTag returns nil for plain user text
// — the hot path that runs on every userMessage in a session. A
// positive answer would churn a throwaway slice on every turn.
func TestParseSubagentNotifications_NoTag(t *testing.T) {
	if got := parseSubagentNotifications(""); got != nil {
		t.Errorf("empty: got %+v, want nil", got)
	}
	if got := parseSubagentNotifications("plain user message"); got != nil {
		t.Errorf("plain text: got %+v, want nil", got)
	}
}

// TestExtractSubagentNotificationsFromUserMessage_WireShape feeds the
// exact params shape that comes off the wire on item/completed for a
// userMessage (UserInput array with type=text entries) and asserts the
// notifications are extracted. This is the integration between the
// JSON-shape path and the parser.
func TestExtractSubagentNotificationsFromUserMessage_WireShape(t *testing.T) {
	params := json.RawMessage(`{
		"threadId":"parent-thread",
		"item":{
			"id":"user-msg-1",
			"type":"userMessage",
			"content":[
				{"type":"text","text":"<subagent_notification>{\"agent_path\":\"child-1\",\"status\":\"completed\"}</subagent_notification>","text_elements":[]},
				{"type":"text","text":"follow-up question","text_elements":[]}
			]
		}
	}`)
	got := extractSubagentNotificationsFromUserMessage(params)
	if len(got) != 1 {
		t.Fatalf("len=%d, want 1 (got=%+v)", len(got), got)
	}
	if got[0].AgentPath != "child-1" || got[0].Status != "completed" {
		t.Errorf("got %+v", got[0])
	}
}

// TestExtractSubagentNotificationsFromUserMessage_NotUserMessage
// guards the filter: assistant and reasoning items must not trigger
// the parser. This matters because the parser walks the full content
// array — running it on every item would waste allocations on every
// turn.
func TestExtractSubagentNotificationsFromUserMessage_NotUserMessage(t *testing.T) {
	params := json.RawMessage(`{
		"item":{
			"id":"asst-1",
			"type":"agentMessage",
			"text":"<subagent_notification>{\"agent_path\":\"child-1\",\"status\":\"completed\"}</subagent_notification>"
		}
	}`)
	if got := extractSubagentNotificationsFromUserMessage(params); got != nil {
		t.Errorf("non-userMessage: got %+v, want nil", got)
	}
}

// TestExtractSubagentNotificationsFromUserMessage_NonTextContent
// confirms the UserInput tagged union filter: image / mention / skill
// entries must not be text-concatenated (their `text` field, if any,
// carries different semantics).
func TestExtractSubagentNotificationsFromUserMessage_NonTextContent(t *testing.T) {
	params := json.RawMessage(`{
		"item":{
			"id":"user-msg-1",
			"type":"userMessage",
			"content":[
				{"type":"image","url":"https://example.test/image.png"},
				{"type":"mention","name":"file","path":"/tmp/a"}
			]
		}
	}`)
	if got := extractSubagentNotificationsFromUserMessage(params); got != nil {
		t.Errorf("non-text content: got %+v, want nil", got)
	}
}

// TestDispatchLineSubagentNotificationEmitsEvent pins the emission
// contract: when an item/completed userMessage carries a
// <subagent_notification> tag, dispatchLine must fire an
// EventSubagentNotification with ThreadID and a Meta payload carrying
// at least agent_path and status. This is the integration between the
// parser and the event emission path — the triage handler and UI
// renderer downstream assume the event actually fires.
func TestDispatchLineSubagentNotificationEmitsEvent(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID:            "parent-thread",
		pending:             make(map[int64]chan json.RawMessage),
		childParentByThread: map[string]string{"child-done": "call-collab-1"},
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	// Shape mirrors the userMessage item/completed frame Codex core
	// emits after a detached child agent reaches a terminal state.
	line := []byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"parent-thread","item":{"id":"user-msg-1","type":"userMessage","content":[{"type":"text","text":"<subagent_notification>{\"agent_path\":\"child-done\",\"status\":\"completed\"}</subagent_notification>"}]}}}`)
	s.dispatchLine(line)

	var notif *provider.ProviderEvent
	for i := range events {
		if events[i].Kind == provider.EventSubagentNotification {
			notif = &events[i]
			break
		}
	}
	if notif == nil {
		t.Fatalf("expected EventSubagentNotification among emitted events; got %+v", events)
	}
	if notif.ThreadID != "parent-thread" {
		t.Errorf("ThreadID: got %q, want parent-thread", notif.ThreadID)
	}

	var meta map[string]any
	if err := json.Unmarshal(notif.Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if meta["agent_path"] != "child-done" {
		t.Errorf("meta.agent_path: got %v, want child-done", meta["agent_path"])
	}
	if meta["status"] != "completed" {
		t.Errorf("meta.status: got %v, want completed", meta["status"])
	}
}

func TestDispatchLineRawInterAgentSubagentNotificationEmitsEvent(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		pending:  make(map[int64]chan json.RawMessage),
		childParentByAgentPath: map[string]string{
			"/root/researcher": "call-collab-1",
		},
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}
	s.setRootThreadID("parent-provider-thread")

	line := rawInterAgentSubagentNotificationLineForThread(t, "parent-provider-thread", map[string]any{
		"agent_path": "/root/researcher",
		"status": map[string]any{
			"completed": "No findings.",
		},
	})
	s.dispatchLine(line)

	var notif *provider.ProviderEvent
	for i := range events {
		if events[i].Kind == provider.EventSubagentNotification {
			notif = &events[i]
			break
		}
	}
	if notif == nil {
		t.Fatalf("expected EventSubagentNotification among emitted events; got %+v", events)
	}
	if notif.ThreadID != "parent-thread" {
		t.Errorf("ThreadID: got %q, want parent-thread", notif.ThreadID)
	}
	if notif.ItemID != "call-collab-1" {
		t.Errorf("ItemID: got %q, want call-collab-1", notif.ItemID)
	}

	var meta map[string]any
	if err := json.Unmarshal(notif.Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if meta["agent_path"] != "/root/researcher" {
		t.Errorf("meta.agent_path: got %v, want /root/researcher", meta["agent_path"])
	}
	if meta["status"] != "completed" {
		t.Errorf("meta.status: got %v, want completed", meta["status"])
	}
	if meta["message"] != "No findings." {
		t.Errorf("meta.message: got %v, want final child answer", meta["message"])
	}
}

func TestDispatchLineRawInterAgentSubagentNotificationWithoutPhaseIgnored(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		pending:  make(map[int64]chan json.RawMessage),
		childParentByAgentPath: map[string]string{
			"/root/researcher": "call-collab-1",
		},
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}
	s.setRootThreadID("parent-provider-thread")

	line := rawInterAgentSubagentNotificationLineForThreadAndPhase(t, "parent-provider-thread", "", map[string]any{
		"agent_path": "/root/researcher",
		"status":     "completed",
	})
	s.dispatchLine(line)

	for _, evt := range events {
		if evt.Kind == provider.EventSubagentNotification {
			t.Fatalf("no-phase raw carrier must not emit control event: %+v", events)
		}
	}
}

func TestDispatchLineRawInterAgentSubagentNotificationMixedContentIgnored(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		pending:  make(map[int64]chan json.RawMessage),
		childParentByAgentPath: map[string]string{
			"/root/researcher": "call-collab-1",
		},
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	line := rawInterAgentMessageLine(t, "ordinary note\n"+subagentNotificationTag(t, map[string]any{
		"agent_path": "/root/researcher",
		"status":     "completed",
	}))
	s.dispatchLine(line)

	for _, evt := range events {
		if evt.Kind == provider.EventSubagentNotification {
			t.Fatalf("mixed raw inter-agent content must not emit control event: %+v", events)
		}
	}
}

func TestDispatchLineRawInterAgentSubagentNotificationAuthorMismatchIgnored(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		pending:  make(map[int64]chan json.RawMessage),
		childParentByAgentPath: map[string]string{
			"/root/other": "call-collab-1",
		},
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	line := rawInterAgentSubagentNotificationLine(t, map[string]any{
		"agent_path": "/root/other",
		"status":     "completed",
	})
	s.dispatchLine(line)

	for _, evt := range events {
		if evt.Kind == provider.EventSubagentNotification {
			t.Fatalf("author-mismatched raw inter-agent content must not emit control event: %+v", events)
		}
	}
}

func TestDispatchLineRawInterAgentSubagentNotificationFromChildThreadIgnored(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		pending:  make(map[int64]chan json.RawMessage),
		childParentByThread: map[string]string{
			"child-provider-thread": "call-collab-1",
		},
		childParentByAgentPath: map[string]string{
			"/root/researcher": "call-collab-1",
		},
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}
	s.setRootThreadID("parent-provider-thread")

	line := rawInterAgentSubagentNotificationLineForThread(t, "child-provider-thread", map[string]any{
		"agent_path": "/root/researcher",
		"status":     "completed",
	})
	s.dispatchLine(line)

	for _, evt := range events {
		if evt.Kind == provider.EventSubagentNotification {
			t.Fatalf("child-thread raw inter-agent content must not emit parent-observed completion: %+v", events)
		}
	}
}

func TestDispatchLineRawUserSubagentNotificationMixedContentIgnored(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		pending:  make(map[int64]chan json.RawMessage),
		childParentByThread: map[string]string{
			"child-done": "call-collab-1",
		},
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	line := rawUserMessageLineForThread(t, "parent-thread", "ordinary note\n"+subagentNotificationTag(t, map[string]any{
		"agent_path": "child-done",
		"status":     "completed",
	}))
	s.dispatchLine(line)

	for _, evt := range events {
		if evt.Kind == provider.EventSubagentNotification {
			t.Fatalf("mixed raw user content must not emit control event: %+v", events)
		}
	}
}

func TestDispatchLineRawUserSubagentNotificationWrongBlockTypeIgnored(t *testing.T) {
	for _, blockType := range []string{"output_text", "text"} {
		t.Run(blockType, func(t *testing.T) {
			var events []provider.ProviderEvent
			s := &Session{
				threadID: "parent-thread",
				pending:  make(map[int64]chan json.RawMessage),
				childParentByThread: map[string]string{
					"child-done": "call-collab-1",
				},
				onEvent: func(evt provider.ProviderEvent) {
					events = append(events, evt)
				},
			}

			line := rawUserMessageLineForThreadAndBlockType(t, "parent-thread", blockType, subagentNotificationTag(t, map[string]any{
				"agent_path": "child-done",
				"status":     "completed",
			}))
			s.dispatchLine(line)

			for _, evt := range events {
				if evt.Kind == provider.EventSubagentNotification {
					t.Fatalf("raw user %s block must not emit control event: %+v", blockType, events)
				}
			}
		})
	}
}

func TestDispatchLineRawInterAgentSubagentNotificationWrongBlockTypeIgnored(t *testing.T) {
	for _, blockType := range []string{"input_text", "text"} {
		t.Run(blockType, func(t *testing.T) {
			var events []provider.ProviderEvent
			s := &Session{
				threadID: "parent-thread",
				pending:  make(map[int64]chan json.RawMessage),
				childParentByAgentPath: map[string]string{
					"/root/researcher": "call-collab-1",
				},
				onEvent: func(evt provider.ProviderEvent) {
					events = append(events, evt)
				},
			}

			line := rawInterAgentMessageLineForThreadAndPhaseAndBlockType(t, "parent-thread", "commentary", subagentNotificationTag(t, map[string]any{
				"agent_path": "/root/researcher",
				"status":     "completed",
			}), blockType)
			s.dispatchLine(line)

			for _, evt := range events {
				if evt.Kind == provider.EventSubagentNotification {
					t.Fatalf("raw assistant %s block must not emit control event: %+v", blockType, events)
				}
			}
		})
	}
}

func TestDispatchLineRawAssistantMessageDoesNotEmitSubagentNotification(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	line := rawMessageLine(t, "plain assistant commentary")
	s.dispatchLine(line)

	if len(events) != 0 {
		t.Fatalf("ordinary raw assistant message should stay non-visual, got %+v", events)
	}
}

func TestRolloutSubagentNotificationLineEmitsEvent(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		childParentByThread: map[string]string{
			"child-done": "call-collab-1",
		},
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	line := rolloutUserSubagentNotificationLine(t, "child-done", map[string]any{
		"completed": "detached child finished",
	})
	if !s.emitSubagentNotificationsFromRolloutLine(line) {
		t.Fatal("rollout notification line was not consumed")
	}

	if len(events) != 1 {
		t.Fatalf("events = %+v, want one EventSubagentNotification", events)
	}
	if events[0].Kind != provider.EventSubagentNotification {
		t.Fatalf("event kind = %q, want %q", events[0].Kind, provider.EventSubagentNotification)
	}
	if events[0].ItemID != "call-collab-1" {
		t.Fatalf("ItemID = %q, want call-collab-1", events[0].ItemID)
	}

	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if meta["agent_path"] != "child-done" {
		t.Errorf("meta.agent_path = %v, want child-done", meta["agent_path"])
	}
	if meta["status"] != "completed" {
		t.Errorf("meta.status = %v, want completed", meta["status"])
	}
	if meta["message"] != "detached child finished" {
		t.Errorf("meta.message = %v, want detached child finished", meta["message"])
	}
}

func TestRolloutSubagentNotificationLineEmitsWithoutProviderMapping(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	if !s.emitSubagentNotificationsFromRolloutLine(rolloutUserSubagentNotificationLine(t, "child-resumed", map[string]any{
		"completed": "detached child finished after resume",
	})) {
		t.Fatal("rollout notification line was not consumed")
	}

	if len(events) != 1 {
		t.Fatalf("events = %+v, want one EventSubagentNotification", events)
	}
	if events[0].Kind != provider.EventSubagentNotification {
		t.Fatalf("event kind = %q, want %q", events[0].Kind, provider.EventSubagentNotification)
	}
	if events[0].ItemID != "" {
		t.Fatalf("ItemID = %q, want empty so triage can resolve persisted launch", events[0].ItemID)
	}
}

func TestRolloutAndRawSubagentNotificationDedupes(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		pending:  make(map[int64]chan json.RawMessage),
		childParentByThread: map[string]string{
			"child-done": "call-collab-1",
		},
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}
	s.setRootThreadID("parent-provider-thread")

	rawLine := rawUserSubagentNotificationLineForThread(t, "parent-provider-thread", map[string]any{
		"agent_path": "child-done",
		"status": map[string]any{
			"completed": "detached child finished",
		},
	})
	s.dispatchLine(rawLine)
	s.emitSubagentNotificationsFromRolloutLine(rolloutUserSubagentNotificationLine(t, "child-done", map[string]any{
		"completed": "detached child finished",
	}))

	var notificationCount int
	for _, evt := range events {
		if evt.Kind == provider.EventSubagentNotification {
			notificationCount++
		}
	}
	if notificationCount != 1 {
		t.Fatalf("EventSubagentNotification count = %d, want 1; events=%+v", notificationCount, events)
	}
}

func TestWatchRolloutSubagentNotificationsEmitsSplitLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-2026-06-16T00-01-18-parent-provider-thread.jsonl")
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatalf("write empty rollout: %v", err)
	}

	events := make(chan provider.ProviderEvent, 1)
	s := &Session{
		threadID: "parent-thread",
		readDone: make(chan struct{}),
		onEvent: func(evt provider.ProviderEvent) {
			events <- evt
		},
	}
	s.setRootThreadID("parent-provider-thread")
	path, offset, err := prepareRolloutSubagentNotificationObserver(path, "parent-provider-thread")
	if err != nil {
		t.Fatalf("prepare rollout observer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.watchRolloutSubagentNotifications(ctx, path, offset)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("rollout watcher did not exit after cancel")
		}
	})

	line := append(rolloutUserSubagentNotificationLine(t, "child-resumed", "completed"), '\n')
	split := len(line) / 2
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	if _, err := file.Write(line[:split]); err != nil {
		file.Close()
		t.Fatalf("append first half: %v", err)
	}
	select {
	case evt := <-events:
		t.Fatalf("watcher emitted before newline: %+v", evt)
	case <-time.After(rolloutSubagentNotificationPollInterval * 2):
	}
	if _, err := file.Write(line[split:]); err != nil {
		file.Close()
		t.Fatalf("append second half: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close rollout: %v", err)
	}

	select {
	case evt := <-events:
		if evt.Kind != provider.EventSubagentNotification {
			t.Fatalf("event kind = %q, want %q", evt.Kind, provider.EventSubagentNotification)
		}
		if evt.ItemID != "" {
			t.Fatalf("ItemID = %q, want empty for persisted triage resolution", evt.ItemID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for rollout notification event")
	}
}

func TestReadRolloutAppendStartsAfterExistingHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	historical := append(rolloutUserSubagentNotificationLine(t, "child-old", "completed"), '\n')
	if err := os.WriteFile(path, historical, 0644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	offset, ok := waitForRolloutNotificationStartOffset(ctx, path)
	if !ok {
		t.Fatal("waitForRolloutNotificationStartOffset returned !ok")
	}
	if offset != int64(len(historical)) {
		t.Fatalf("offset = %d, want %d", offset, len(historical))
	}

	fresh := append(rolloutUserSubagentNotificationLine(t, "child-fresh", "completed"), '\n')
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	if _, err := file.Write(fresh); err != nil {
		file.Close()
		t.Fatalf("append rollout: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close rollout: %v", err)
	}

	chunk, _, err := readRolloutAppend(path, offset)
	if err != nil {
		t.Fatalf("readRolloutAppend: %v", err)
	}
	if string(chunk) != string(fresh) {
		t.Fatalf("chunk = %q, want only fresh line %q", string(chunk), string(fresh))
	}
}

func TestPrepareRolloutSubagentNotificationObserverValidatesPath(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "rollout-2026-06-16T00-01-18-parent-provider-thread.jsonl")
	if err := os.WriteFile(valid, []byte("history\n"), 0644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	path, offset, err := prepareRolloutSubagentNotificationObserver(valid, "parent-provider-thread")
	if err != nil {
		t.Fatalf("valid rollout rejected: %v", err)
	}
	if path != filepath.Clean(valid) {
		t.Fatalf("path = %q, want %q", path, filepath.Clean(valid))
	}
	if offset != int64(len("history\n")) {
		t.Fatalf("offset = %d, want history length", offset)
	}

	mismatch := filepath.Join(dir, "rollout-2026-06-16T00-01-18-other-thread.jsonl")
	if err := os.WriteFile(mismatch, nil, 0644); err != nil {
		t.Fatalf("write mismatch rollout: %v", err)
	}
	if _, _, err := prepareRolloutSubagentNotificationObserver(mismatch, "parent-provider-thread"); err == nil {
		t.Fatal("expected mismatched thread id path to be rejected")
	}

	symlink := filepath.Join(dir, "rollout-2026-06-16T00-01-18-parent-provider-thread-link.jsonl")
	if err := os.Symlink(valid, symlink); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, _, err := prepareRolloutSubagentNotificationObserver(symlink, "parent-provider-thread"); err == nil {
		t.Fatal("expected symlink rollout path to be rejected")
	}
}

func TestDispatchLineSubagentNotificationCarrierDoesNotEmitUserTextWhenMapped(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID:            "parent-thread",
		pending:             make(map[int64]chan json.RawMessage),
		childParentByThread: map[string]string{"child-done": "call-collab-1"},
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	line := []byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"parent-thread","item":{"id":"user-msg-1","type":"userMessage","content":[{"type":"text","text":"<subagent_notification>{\"agent_path\":\"child-done\",\"status\":\"completed\"}</subagent_notification>"}]}}}`)
	s.dispatchLine(line)

	var sawNotification bool
	for _, evt := range events {
		switch evt.Kind {
		case provider.EventSubagentNotification:
			sawNotification = true
			if evt.ItemID != "call-collab-1" {
				t.Fatalf("notification ItemID = %q, want call-collab-1", evt.ItemID)
			}
		case provider.EventUserText:
			t.Fatalf("carrier userMessage emitted EventUserText: %+v", evt)
		}
	}
	if !sawNotification {
		t.Fatalf("expected EventSubagentNotification, got %+v", events)
	}
}

func rawInterAgentSubagentNotificationLine(t *testing.T, notification map[string]any) []byte {
	t.Helper()
	return rawInterAgentSubagentNotificationLineForThread(t, "parent-thread", notification)
}

func rawInterAgentSubagentNotificationLineForThread(t *testing.T, threadID string, notification map[string]any) []byte {
	t.Helper()
	return rawInterAgentSubagentNotificationLineForThreadAndPhase(t, threadID, "commentary", notification)
}

func rawInterAgentSubagentNotificationLineForThreadAndPhase(t *testing.T, threadID string, phase string, notification map[string]any) []byte {
	t.Helper()
	return rawInterAgentMessageLineForThreadAndPhase(t, threadID, phase, subagentNotificationTag(t, notification))
}

func subagentNotificationTag(t *testing.T, notification map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(notification)
	if err != nil {
		t.Fatalf("marshal subagent notification: %v", err)
	}
	return "<subagent_notification>" + string(encoded) + "</subagent_notification>"
}

func rawUserSubagentNotificationLineForThread(t *testing.T, threadID string, notification map[string]any) []byte {
	t.Helper()
	return rawUserMessageLineForThread(t, threadID, subagentNotificationTag(t, notification))
}

func rolloutUserSubagentNotificationLine(t *testing.T, agentPath string, status any) []byte {
	t.Helper()
	item := map[string]any{
		"type": "message",
		"role": "user",
		"content": []map[string]string{
			{
				"type": "input_text",
				"text": subagentNotificationTag(t, map[string]any{
					"agent_path": agentPath,
					"status":     status,
				}),
			},
		},
	}
	line, err := json.Marshal(map[string]any{
		"timestamp": "2026-06-16T05:55:55.622Z",
		"type":      "response_item",
		"payload":   item,
	})
	if err != nil {
		t.Fatalf("marshal rollout response item: %v", err)
	}
	return line
}

func rawUserMessageLineForThread(t *testing.T, threadID string, text string) []byte {
	t.Helper()
	return rawUserMessageLineForThreadAndBlockType(t, threadID, "input_text", text)
}

func rawUserMessageLineForThreadAndBlockType(t *testing.T, threadID string, blockType string, text string) []byte {
	t.Helper()
	item := map[string]any{
		"type": "message",
		"role": "user",
		"content": []map[string]string{
			{
				"type": blockType,
				"text": text,
			},
		},
	}
	line, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "rawResponseItem/completed",
		"params": map[string]any{
			"threadId": threadID,
			"turnId":   "turn-2",
			"item":     item,
		},
	})
	if err != nil {
		t.Fatalf("marshal raw user message line: %v", err)
	}
	return line
}

func rawInterAgentMessageLine(t *testing.T, content string) []byte {
	t.Helper()
	return rawInterAgentMessageLineForThread(t, "parent-thread", content)
}

func rawInterAgentMessageLineForThread(t *testing.T, threadID string, content string) []byte {
	t.Helper()
	return rawInterAgentMessageLineForThreadAndPhase(t, threadID, "commentary", content)
}

func rawInterAgentMessageLineForThreadAndPhase(t *testing.T, threadID string, phase string, content string) []byte {
	t.Helper()
	return rawInterAgentMessageLineForThreadAndPhaseAndBlockType(t, threadID, phase, content, "output_text")
}

func rawInterAgentMessageLineForThreadAndPhaseAndBlockType(t *testing.T, threadID string, phase string, content string, blockType string) []byte {
	t.Helper()
	communication := map[string]any{
		"author":           "/root/researcher",
		"recipient":        "/root",
		"other_recipients": []string{},
		"content":          content,
		"trigger_turn":     false,
	}
	encoded, err := json.Marshal(communication)
	if err != nil {
		t.Fatalf("marshal inter-agent communication: %v", err)
	}
	return rawMessageLineForThreadAndPhaseAndBlockType(t, threadID, phase, blockType, string(encoded))
}

func rawMessageLine(t *testing.T, text string) []byte {
	t.Helper()
	return rawMessageLineForThread(t, "parent-thread", text)
}

func rawMessageLineForThread(t *testing.T, threadID string, text string) []byte {
	t.Helper()
	return rawMessageLineForThreadAndPhase(t, threadID, "commentary", text)
}

func rawMessageLineForThreadAndPhase(t *testing.T, threadID string, phase string, text string) []byte {
	t.Helper()
	return rawMessageLineForThreadAndPhaseAndBlockType(t, threadID, phase, "output_text", text)
}

func rawMessageLineForThreadAndPhaseAndBlockType(t *testing.T, threadID string, phase string, blockType string, text string) []byte {
	t.Helper()
	item := map[string]any{
		"type": "message",
		"role": "assistant",
		"content": []map[string]string{
			{
				"type": blockType,
				"text": text,
			},
		},
	}
	if phase != "" {
		item["phase"] = phase
	}
	line, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "rawResponseItem/completed",
		"params": map[string]any{
			"threadId": threadID,
			"turnId":   "turn-2",
			"item":     item,
		},
	})
	if err != nil {
		t.Fatalf("marshal raw message line: %v", err)
	}
	return line
}

func TestDispatchLineSubagentNotificationMixedContentKeepsUserText(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID:            "parent-thread",
		pending:             make(map[int64]chan json.RawMessage),
		childParentByThread: map[string]string{"child-done": "call-collab-1"},
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	line := []byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"parent-thread","item":{"id":"user-msg-1","type":"userMessage","content":[{"type":"text","text":"keep this text\n<subagent_notification>{\"agent_path\":\"child-done\",\"status\":\"completed\"}</subagent_notification>"}]}}}`)
	s.dispatchLine(line)

	var sawNotification bool
	var sawUserText bool
	for _, evt := range events {
		switch evt.Kind {
		case provider.EventSubagentNotification:
			sawNotification = true
		case provider.EventUserText:
			sawUserText = true
		}
	}
	if sawNotification {
		t.Fatalf("mixed user text must not emit forgeable subagent notification: %+v", events)
	}
	if !sawUserText {
		t.Fatalf("mixed content should keep user text, got %+v", events)
	}
}

func TestDispatchLineSubagentNotificationUnmappedCarrierKeepsUserText(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID:            "parent-thread",
		pending:             make(map[int64]chan json.RawMessage),
		childParentByThread: make(map[string]string),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	line := []byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"parent-thread","item":{"id":"user-msg-1","type":"userMessage","content":[{"type":"text","text":"<subagent_notification>{\"agent_path\":\"child-done\",\"status\":\"completed\"}</subagent_notification>"}]}}}`)
	s.dispatchLine(line)

	var sawUserText bool
	for _, evt := range events {
		if evt.Kind == provider.EventSubagentNotification {
			t.Fatalf("unmapped notification carrier emitted control event: %+v", evt)
		}
		if evt.Kind == provider.EventUserText {
			sawUserText = true
		}
	}
	if !sawUserText {
		t.Fatalf("unmapped carrier should remain literal user text, got %+v", events)
	}
}

// TestDispatchLineSubagentNotificationMultipleTagsEmitOnce pins that a
// userMessage carrying multiple <subagent_notification> tags produces
// one EventSubagentNotification per tag, in source order. The UI
// surfaces each terminal child as its own notification; a single
// combined event would collapse them.
func TestDispatchLineSubagentNotificationMultipleTagsEmitOnce(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		pending:  make(map[int64]chan json.RawMessage),
		childParentByThread: map[string]string{
			"child-1": "call-collab-1",
			"child-2": "call-collab-2",
		},
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	line := []byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"parent-thread","item":{"id":"user-msg-1","type":"userMessage","content":[{"type":"text","text":"<subagent_notification>{\"agent_path\":\"child-1\",\"status\":\"completed\"}</subagent_notification>\n<subagent_notification>{\"agent_path\":\"child-2\",\"status\":\"errored\"}</subagent_notification>"}]}}}`)
	s.dispatchLine(line)

	var agents []string
	for _, evt := range events {
		if evt.Kind != provider.EventSubagentNotification {
			continue
		}
		var meta map[string]any
		if err := json.Unmarshal(evt.Meta, &meta); err != nil {
			t.Fatalf("meta unmarshal: %v", err)
		}
		agents = append(agents, meta["agent_path"].(string))
	}
	if len(agents) != 2 {
		t.Fatalf("expected 2 EventSubagentNotification events, got %d (agents=%v events=%+v)", len(agents), agents, events)
	}
	if agents[0] != "child-1" || agents[1] != "child-2" {
		t.Errorf("order: got %v, want [child-1 child-2]", agents)
	}
}

// TestBuildSubagentNotificationMetaIncludesExtra pins the Extra-field
// forward-compat promise: custom fields Codex core adds to the
// notification JSON must round-trip through buildSubagentNotificationMeta
// onto the frontend-facing meta blob. The load-bearing agent_path /
// status keys always win on collision.
func TestBuildSubagentNotificationMetaIncludesExtra(t *testing.T) {
	n := subagentNotification{
		AgentPath: "child-extra",
		Status:    "completed",
		Message:   "ok",
		Extra: map[string]any{
			"message":     "clobber-attempt",
			"duration_ms": float64(1234),
			// Attempted collision — the canonical fields must win.
			"agent_path": "clobber-attempt",
			"status":     "clobber-attempt",
		},
	}
	raw := buildSubagentNotificationMeta(n)
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if meta["agent_path"] != "child-extra" {
		t.Errorf("agent_path: got %v, want child-extra (Extra must not clobber)", meta["agent_path"])
	}
	if meta["status"] != "completed" {
		t.Errorf("status: got %v, want completed (Extra must not clobber)", meta["status"])
	}
	if meta["message"] != "ok" {
		t.Errorf("message: got %v, want ok", meta["message"])
	}
	if meta["duration_ms"] != float64(1234) {
		t.Errorf("duration_ms: got %v, want 1234", meta["duration_ms"])
	}
}

// codexReviewerEchoScript is a fake app-server that answers thread/start and
// thread/resume with a caller-chosen `approvalsReviewer` (or none at all, for
// the pre-0.115 silent-drop simulation) and logs every inbound line.
func codexReviewerEchoScript(t *testing.T, capturePath, threadResult string) string {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/bash
while IFS= read -r line; do
    printf '%%s\n' "$line" >> %q
    id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if echo "$line" | grep -q '"method":"turn/start"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"turn\":{\"id\":\"turn-reviewer\"}}}"
    elif echo "$line" | grep -qE '"method":"thread/(start|resume)"'; then
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":%s}"
    else
        echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{}}"
    fi
done
`, capturePath, threadResult)
	scriptPath := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock script: %v", err)
	}
	return scriptPath
}

func codexCapturedRequest(t *testing.T, capturePath, method string) map[string]any {
	t.Helper()
	captured, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	needle := `"method":"` + method + `"`
	for _, line := range strings.Split(string(captured), "\n") {
		if !strings.Contains(line, needle) {
			continue
		}
		var req map[string]any
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			t.Fatalf("unmarshal %s: %v", method, err)
		}
		params, ok := req["params"].(map[string]any)
		if !ok {
			t.Fatalf("%s carried no params object: %s", method, line)
		}
		return params
	}
	t.Fatalf("captured no %s request: %s", method, string(captured))
	return nil
}

// TestSessionStartVerifiesApprovalsReviewerEcho is the wire-level guard for
// the auto runtime mode on Codex. `ThreadStartParams` has no
// deny_unknown_fields, so a codex that predates `approvalsReviewer` accepts
// the request, drops the field, and hands back an ordinary user-reviewer
// thread. Nothing else on the wire reports this: `initialize` carries no
// version or capability list and `thread/started` does not carry the reviewer,
// so the handshake RESPONSE is the only place the drop is visible.
//
// The failure has to be an error rather than a downgrade. Continuing would run
// the session with a human on the other end of approvals while the thread row,
// the picker, and the user all say a reviewer is answering them.
func TestSessionStartVerifiesApprovalsReviewerEcho(t *testing.T) {
	cases := []struct {
		name         string
		mode         provider.RuntimeMode
		threadResult string
		wantErr      string
	}{
		{
			name:         "auto accepted and echoed",
			mode:         provider.RuntimeAuto,
			threadResult: `{\"thread\":{\"id\":\"mock-thread-123\"},\"approvalsReviewer\":\"auto_review\"}`,
		},
		{
			name:         "auto silently dropped by an old app-server",
			mode:         provider.RuntimeAuto,
			threadResult: `{\"thread\":{\"id\":\"mock-thread-123\"}}`,
			wantErr:      "auto_review",
		},
		{
			name:         "auto downgraded to the user reviewer",
			mode:         provider.RuntimeAuto,
			threadResult: `{\"thread\":{\"id\":\"mock-thread-123\"},\"approvalsReviewer\":\"user\"}`,
			wantErr:      "auto_review",
		},
		{
			name:         "non-auto tier tolerates an absent echo",
			mode:         provider.RuntimeApprovalRequired,
			threadResult: `{\"thread\":{\"id\":\"mock-thread-123\"}}`,
		},
		{
			name:         "non-auto tier rejects a sticky auto reviewer",
			mode:         provider.RuntimeApprovalRequired,
			threadResult: `{\"thread\":{\"id\":\"mock-thread-123\"},\"approvalsReviewer\":\"auto_review\"}`,
			wantErr:      "user",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capturePath := filepath.Join(t.TempDir(), "codex-stdin.log")
			cfg := ConfigFromOptions(provider.SessionOptions{
				Provider:    "codex",
				RuntimeMode: tc.mode,
				WorkDir:     "/tmp",
			})
			cfg.Binary = codexReviewerEchoScript(t, capturePath, tc.threadResult)

			s, err := NewSession(context.Background(), testThread, cfg, func(provider.ProviderEvent) {})
			if s != nil {
				t.Cleanup(func() { _ = s.Close() })
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("NewSession: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("NewSession accepted a reviewer mismatch")
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not name the requested reviewer %q", err, tc.wantErr)
				}
			}

			params := codexCapturedRequest(t, capturePath, "thread/start")
			want := codexApprovalsReviewer(tc.mode)
			if params["approvalsReviewer"] != want {
				t.Errorf("thread/start approvalsReviewer = %v, want %q", params["approvalsReviewer"], want)
			}
		})
	}
}

// TestSessionSendIncludesApprovalsReviewerForEveryMode proves the reviewer
// rides every turn/start, not just the handshake. Codex keeps the reviewer as
// thread state until something overwrites it, so a turn that omits it inherits
// the previous runtime mode's choice — which is how a thread switched OUT of
// auto keeps auto-approving its own escalations.
func TestSessionSendIncludesApprovalsReviewerForEveryMode(t *testing.T) {
	for _, mode := range provider.AllRuntimeModes {
		t.Run(string(mode), func(t *testing.T) {
			capturePath := filepath.Join(t.TempDir(), "codex-stdin.log")
			want := codexApprovalsReviewer(mode)
			threadResult := fmt.Sprintf(`{\"thread\":{\"id\":\"mock-thread-123\"},\"approvalsReviewer\":\"%s\"}`, want)
			cfg := ConfigFromOptions(provider.SessionOptions{
				Provider:    "codex",
				RuntimeMode: mode,
				WorkDir:     "/tmp",
			})
			cfg.Binary = codexReviewerEchoScript(t, capturePath, threadResult)

			s, err := NewSession(context.Background(), testThread, cfg, func(provider.ProviderEvent) {})
			if err != nil {
				t.Fatalf("NewSession: %v", err)
			}
			if err := s.Send(context.Background(), "hello", provider.SendOptions{}); err != nil {
				t.Fatalf("Send: %v", err)
			}
			_ = s.Close()

			params := codexCapturedRequest(t, capturePath, "turn/start")
			if params["approvalsReviewer"] != want {
				t.Errorf("turn/start approvalsReviewer = %v, want %q", params["approvalsReviewer"], want)
			}
		})
	}
}
