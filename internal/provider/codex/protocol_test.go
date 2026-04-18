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

// TestClassifyNotification_NeverPopulatesParentToolUseID asserts the
// intentional absence of parent-tool linkage on Codex events. Codex's
// app-server protocol does not expose a parentToolUseId concept, so
// every ProviderEvent we emit must leave the field empty. If a future
// Codex release adds this field, extract it in protocol.go and update
// this test.
func TestClassifyNotification_NeverPopulatesParentToolUseID(t *testing.T) {
	// One params blob per notification method; values where a naive
	// implementation might look for a parent id are present but wrong
	// shape ("parentToolUseId" was never part of Codex's contract).
	cases := []struct {
		method string
		params string
	}{
		{"turn/started", `{"turn":{"id":"t1"}, "parentToolUseId":"bogus"}`},
		{"turn/completed", `{"turn":{"id":"t1","status":"completed"}, "parentToolUseId":"bogus"}`},
		{"turn/aborted", `{"turn":{"id":"t1"}, "parentToolUseId":"bogus"}`},
		{"turn/diff/updated", `{"diff":"diff --git a/x b/x", "parentToolUseId":"bogus"}`},
		{"item/started", `{"item":{"id":"i1","type":"command_execution"}, "parentToolUseId":"bogus"}`},
		{"item/updated", `{"item":{"id":"i1","type":"command_execution"}, "parentToolUseId":"bogus"}`},
		{"item/completed", `{"item":{"id":"i1","type":"command_execution"}, "parentToolUseId":"bogus"}`},
		{"item/agentMessage/delta", `{"delta":"hi", "parentToolUseId":"bogus"}`},
		{"item/commandExecution/outputDelta", `{"delta":"out", "parentToolUseId":"bogus"}`},
		{"item/fileChange/outputDelta", `{"delta":"patch", "parentToolUseId":"bogus"}`},
		{"item/reasoning/textDelta", `{"delta":"think", "parentToolUseId":"bogus"}`},
		{"thread/tokenUsage/updated", `{"usage":{"inputTokens":1}, "parentToolUseId":"bogus"}`},
		{"thread/name/updated", `{"threadName":"n", "parentToolUseId":"bogus"}`},
		{"thread/compacted", `{"parentToolUseId":"bogus"}`},
		{"error", `{"error":{"message":"oops"}, "parentToolUseId":"bogus"}`},
		{"turn/plan/updated", `{"parentToolUseId":"bogus"}`},
		{"serverRequest/resolved", `{"providerRequestId":"req-1", "parentToolUseId":"bogus"}`},
		{"account/rateLimits/updated", `{"parentToolUseId":"bogus"}`},
		{"model/rerouted", `{"toModel":"x", "parentToolUseId":"bogus"}`},
	}

	for _, tc := range cases {
		t.Run(tc.method, func(t *testing.T) {
			events := ClassifyNotification("t1", tc.method, json.RawMessage(tc.params))
			for i, evt := range events {
				if evt.ParentToolUseID != "" {
					t.Errorf("%s event %d: ParentToolUseID = %q, want empty (Codex has no parent-tool linkage)",
						tc.method, i, evt.ParentToolUseID)
				}
			}
		})
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

// -- terminalInteraction / mcpToolCall progress tests --

// TestClassifyNotification_TerminalInteraction exercises
// item/commandExecution/terminalInteraction, which Codex emits when a
// command prompts for input (sudo password, interactive REPL, etc.). We
// route it as EventToolProgress with a subtype discriminator so triage
// can branch without yet owning a dedicated EventKind for it. Wire
// format in codex-source/schema/typescript/v2/TerminalInteractionNotification.ts.
func TestClassifyNotification_TerminalInteraction(t *testing.T) {
	params := json.RawMessage(
		`{"threadId":"th-1","turnId":"t1","itemId":"cmd-1","processId":"pid-42","stdin":"password:"}`,
	)
	events := ClassifyNotification("th-1", "item/commandExecution/terminalInteraction", params)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	evt := events[0]
	if evt.Kind != provider.EventToolProgress {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventToolProgress)
	}
	if evt.ItemID != "cmd-1" {
		t.Errorf("itemID: got %q, want %q", evt.ItemID, "cmd-1")
	}
	if evt.TurnID != "t1" {
		t.Errorf("turnID: got %q, want %q", evt.TurnID, "t1")
	}
	var meta map[string]any
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["subtype"] != "terminal_interaction" {
		t.Errorf("meta.subtype: got %v, want %q", meta["subtype"], "terminal_interaction")
	}
	if meta["processId"] != "pid-42" {
		t.Errorf("meta.processId: got %v, want %q", meta["processId"], "pid-42")
	}
	if meta["stdin"] != "password:" {
		t.Errorf("meta.stdin: got %v, want %q", meta["stdin"], "password:")
	}
}

// TestClassifyNotification_McpToolCallProgress wraps the
// item/mcpToolCall/progress notification — Codex forwards a progress
// message string from the MCP server. Wire format in
// codex-source/schema/typescript/v2/McpToolCallProgressNotification.ts.
func TestClassifyNotification_McpToolCallProgress(t *testing.T) {
	params := json.RawMessage(
		`{"threadId":"th-1","turnId":"t1","itemId":"mcp-1","message":"Indexed 47/100 files"}`,
	)
	events := ClassifyNotification("th-1", "item/mcpToolCall/progress", params)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	evt := events[0]
	if evt.Kind != provider.EventToolProgress {
		t.Errorf("kind: got %q, want %q", evt.Kind, provider.EventToolProgress)
	}
	if evt.ItemID != "mcp-1" {
		t.Errorf("itemID: got %q, want %q", evt.ItemID, "mcp-1")
	}
	var meta map[string]any
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["subtype"] != "mcp_progress" {
		t.Errorf("meta.subtype: got %v, want %q", meta["subtype"], "mcp_progress")
	}
	if meta["message"] != "Indexed 47/100 files" {
		t.Errorf("meta.message: got %v, want %q", meta["message"], "Indexed 47/100 files")
	}
}

// -- turn/completed surface test --

// TestClassifyNotification_TurnCompletedStatus asserts that turn.status
// (TurnStatus: "completed" | "interrupted" | "failed" | "inProgress")
// is lifted into the turn-complete event's Meta.
func TestClassifyNotification_TurnCompletedStatus(t *testing.T) {
	statuses := []string{"completed", "interrupted", "failed", "inProgress"}
	for _, status := range statuses {
		t.Run(status, func(t *testing.T) {
			params := json.RawMessage(
				`{"turn":{"id":"t1","status":"` + status + `"}}`,
			)
			events := ClassifyNotification("th-1", "turn/completed", params)
			// Find the turn-complete event (failed status also emits a
			// paired EventError, so we can't just take events[0]).
			var found provider.ProviderEvent
			for _, e := range events {
				if e.Kind == provider.EventTurnComplete {
					found = e
					break
				}
			}
			if found.Kind == "" {
				t.Fatalf("no EventTurnComplete in events=%+v", events)
			}
			var meta map[string]any
			if err := json.Unmarshal(found.Meta, &meta); err != nil {
				t.Fatalf("unmarshal meta: %v", err)
			}
			if meta["turn_status"] != status {
				t.Errorf("meta.turn_status: got %v, want %q (meta=%+v)", meta["turn_status"], status, meta)
			}
		})
	}
}
