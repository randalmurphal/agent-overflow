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

// TestClassifyNotification_ItemUpdatedIsPhantom pins that the Codex
// app-server wire protocol has NO `item/updated` method — it only
// emits `item/started` and `item/completed`. Reference:
// /Users/randy/repos/codex-source/codex-rs/app-server-protocol/schema/typescript/ServerNotification.ts.
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

func TestClassifyNotification_CollabSpawnUsesCollabAgentType(t *testing.T) {
	params := json.RawMessage(`{"item":{"id":"call-1","type":"collabAgentToolCall","tool":"spawnAgent","prompt":"Refactor auth","receiverThreadIds":["child-1"],"status":"completed"}}`)
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

func TestClassifyNotification_TerminalInteractionDropped(t *testing.T) {
	params := json.RawMessage(
		`{"threadId":"th-1","turnId":"t1","itemId":"cmd-1","processId":"pid-42","stdin":"password:"}`,
	)
	events := ClassifyNotification("th-1", "item/commandExecution/terminalInteraction", params)
	if len(events) != 0 {
		t.Fatalf("expected terminalInteraction to be dropped, got %d event(s)", len(events))
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
