package triage

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// findUpsertedItems pulls every store.Item that triage published as a
// provider:item_event upsert, in emission order. Tests assert against
// this slice to verify the lifecycle item flows into the frontend's
// reconciliation pipeline as it would in production.
func findUpsertedItems(emissions []emitted) []store.Item {
	return filterItemEventUpserts(emissions)
}

// findItemsByKind returns persisted items of the given kind for a thread.
// Convenience wrapper over ListItems used by every lifecycle test.
func findItemsByKind(t *testing.T, st *store.Store, threadID, kind string) []store.Item {
	t.Helper()
	items, err := st.ListItems(threadID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	var out []store.Item
	for _, it := range items {
		if it.Kind == kind {
			out = append(out, it)
		}
	}
	return out
}

// TestToolStartPersistsLifecycleItem covers the baseline contract: an
// EventToolStart for a generic tool (Bash, no file_change semantics)
// produces exactly one tool_call item with status=running, the original
// itemID as the row id, and an empty payload (rich body lands on
// completion if at all).
func TestToolStartPersistsLifecycleItem(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	scopedID := "tool-1"

	meta, _ := json.Marshal(map[string]any{
		"toolName": "Bash",
		"input":    map[string]any{"command": "ls -la"},
	})

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "tool-1",
		ItemType:  "Bash",
		Meta:      meta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle: %v", err)
	}

	items := findItemsByKind(t, st, "t1", itemKindToolCall)
	if len(items) != 1 {
		t.Fatalf("expected 1 tool_call item, got %d", len(items))
	}
	item := items[0]
	if item.ID != scopedID {
		t.Errorf("item.ID = %q, want %q", item.ID, scopedID)
	}
	if item.Status != statusRunning {
		t.Errorf("item.Status = %q, want %q", item.Status, statusRunning)
	}
	if item.IsBackground {
		t.Error("item.IsBackground = true, want false (no flag in meta)")
	}
	if !strings.Contains(item.Summary, "Bash") {
		t.Errorf("item.Summary = %q, want it to contain tool name", item.Summary)
	}
	if !strings.Contains(item.Summary, "ls -la") {
		t.Errorf("item.Summary = %q, want it to contain command preview", item.Summary)
	}
	if item.PayloadID != "" {
		t.Errorf("item.PayloadID = %q, want empty at launch", item.PayloadID)
	}
	var persistedMeta map[string]any
	if err := json.Unmarshal([]byte(item.Meta), &persistedMeta); err != nil {
		t.Fatalf("item.Meta invalid JSON: %v", err)
	}
	input, ok := persistedMeta["input"].(map[string]any)
	if !ok {
		t.Fatalf("item.Meta input missing or wrong type: %#v", persistedMeta["input"])
	}
	if input["command"] != "ls -la" {
		t.Errorf("item.Meta input.command = %v, want ls -la", input["command"])
	}

	upserted := findUpsertedItems(*emissions)
	if len(upserted) != 1 || upserted[0].ID != scopedID {
		t.Errorf("expected 1 upsert for %s, got %+v", scopedID, upserted)
	}
}

func TestToolStartPersistsCodexToolPreviewSummary(t *testing.T) {
	cases := []struct {
		name        string
		itemID      string
		itemType    string
		toolName    string
		input       map[string]any
		wantTool    string
		wantPreview string
	}{
		{
			name:        "web search",
			itemID:      "web-1",
			itemType:    "webSearch",
			toolName:    "WebSearch",
			input:       map[string]any{"query": "codex app-server webSearch"},
			wantTool:    "WebSearch",
			wantPreview: "codex app-server webSearch",
		},
		{
			name:        "mcp tool",
			itemID:      "mcp-1",
			itemType:    "mcpToolCall",
			toolName:    "MCP/lookup",
			input:       map[string]any{"description": "docs/lookup"},
			wantTool:    "MCP/lookup",
			wantPreview: "docs/lookup",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, st, _ := newTestRouter(t)
			createTestThread(t, st, "t1")

			meta, _ := json.Marshal(map[string]any{
				"toolName": tc.toolName,
				"input":    tc.input,
			})
			if err := router.Handle(provider.ProviderEvent{
				Kind:      provider.EventToolStart,
				ThreadID:  "t1",
				ItemID:    tc.itemID,
				ItemType:  tc.itemType,
				Meta:      meta,
				Timestamp: time.Now(),
			}); err != nil {
				t.Fatalf("handle: %v", err)
			}

			item, found, err := st.GetThreadItem("t1", tc.itemID)
			if err != nil || !found {
				t.Fatalf("item missing: found=%v err=%v", found, err)
			}
			if item.ToolName != tc.wantTool {
				t.Fatalf("ToolName = %q, want %q", item.ToolName, tc.wantTool)
			}
			if !strings.Contains(item.Summary, tc.wantTool) {
				t.Fatalf("Summary = %q, want tool name %q", item.Summary, tc.wantTool)
			}
			if !strings.Contains(item.Summary, tc.wantPreview) {
				t.Fatalf("Summary = %q, want preview %q", item.Summary, tc.wantPreview)
			}
		})
	}
}

func TestToolStartSplitsAssistantTextAroundVisibleToolRow(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   "before tool",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("first text delta: %v", err)
	}

	meta, _ := json.Marshal(map[string]any{
		"toolName": "Bash",
		"input":    map[string]any{"command": "pwd"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "tool-boundary",
		ItemType:  "Bash",
		Meta:      meta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   "after tool",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("second text delta: %v", err)
	}

	items, err := st.ListTurnItems("t1", 0)
	if err != nil {
		t.Fatalf("list turn items: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("expected text, tool, text; got %d items: %+v", len(items), items)
	}
	if items[0].Kind != "assistant_text" || items[0].Summary != "before tool" {
		t.Fatalf("first item = (%q, %q), want pre-tool assistant text", items[0].Kind, items[0].Summary)
	}
	if items[1].Kind != itemKindToolCall || items[1].ID != "tool-boundary" {
		t.Fatalf("second item = (%q, %q), want tool_call tool-boundary", items[1].Kind, items[1].ID)
	}
	if items[2].Kind != "assistant_text" || items[2].Summary != "after tool" {
		t.Fatalf("third item = (%q, %q), want post-tool assistant text", items[2].Kind, items[2].Summary)
	}
}

func TestCodexCompletionOnlyControlToolSplitsAssistantText(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	if err := st.UpdateProvider("t1", "codex"); err != nil {
		t.Fatalf("set provider: %v", err)
	}
	seedOpenTurn(t, router, st, "t1", 0)

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   "closing agents now",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("first text delta: %v", err)
	}

	meta, _ := json.Marshal(map[string]any{
		"toolName": "close_agent",
		"input": map[string]any{
			"tool":              "close_agent",
			"receiverThreadIds": []string{"child-1"},
		},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "close-agent-1",
		ItemType: "close_agent", Content: "closed child-1", Meta: meta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("close completion: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   "final answer",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("second text delta: %v", err)
	}

	items, err := st.ListTurnItems("t1", 0)
	if err != nil {
		t.Fatalf("list turn items: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("expected text, close tool, text; got %d items: %+v", len(items), items)
	}
	if items[0].Kind != itemKindAssistantText || items[0].Summary != "closing agents now" {
		t.Fatalf("first item = (%q, %q), want pre-close assistant text", items[0].Kind, items[0].Summary)
	}
	if items[1].Kind != itemKindToolCall || items[1].ID != "close-agent-1" || items[1].ToolName != "close_agent" {
		t.Fatalf("second item = (%q, %q, %q), want close_agent tool row", items[1].Kind, items[1].ID, items[1].ToolName)
	}
	if items[2].Kind != itemKindAssistantText || items[2].Summary != "final answer" {
		t.Fatalf("third item = (%q, %q), want post-close assistant text", items[2].Kind, items[2].Summary)
	}
}

// TestToolStartCarriesBackgroundFlag verifies the is_background bit
// rides through Meta into the persisted row. Claude sets this from
// run_in_background; Codex's classifier sets it via the same key.
func TestToolStartCarriesBackgroundFlag(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	meta, _ := json.Marshal(map[string]any{
		"toolName":      "Bash",
		"input":         map[string]any{"command": "long-running-job"},
		"is_background": true,
	})

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-1",
		ItemType: "Bash", Meta: meta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle: %v", err)
	}

	items := findItemsByKind(t, st, "t1", itemKindToolCall)
	if len(items) != 1 || !items[0].IsBackground {
		t.Fatalf("expected 1 background tool_call, got %+v", items)
	}
}

// TestToolCompleteFlipsInlineStatus covers the inline path: a non-
// background tool_call ends as the same row with status=completed and
// no extra items appended.
func TestToolCompleteFlipsInlineStatus(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	startMeta, _ := json.Marshal(map[string]any{"toolName": "Bash"})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "tool-2",
		ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	completeMeta, _ := json.Marshal(map[string]any{"exit_code": 0})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "tool-2",
		Meta: completeMeta, Content: "stdout body", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	items := findItemsByKind(t, st, "t1", itemKindToolCall)
	if len(items) != 1 {
		t.Fatalf("expected 1 tool_call (no completion sibling for inline), got %d", len(items))
	}
	if items[0].Status != statusCompleted {
		t.Errorf("Status = %q, want completed", items[0].Status)
	}
	dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(dones) != 0 {
		t.Errorf("inline completion must not append background_done, got %d", len(dones))
	}

	// Two upserts: one at launch, one at completion. Both for the same id.
	upserted := findUpsertedItems(*emissions)
	if len(upserted) != 2 {
		t.Fatalf("expected 2 upserts (launch + completion), got %d", len(upserted))
	}
	for _, item := range upserted {
		if item.ID != "tool-2" {
			t.Errorf("upsert id %q, want %q", item.ID, "tool-2")
		}
	}
}

// TestToolCompleteOnErroredFlipsToErrored covers the error transition.
// The exit_code-or-is_error fork in completionStatus picks errored over
// completed; the summary picks up an "(error)" suffix for the row label.
func TestToolCompleteOnErroredFlipsToErrored(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	startMeta, _ := json.Marshal(map[string]any{"toolName": "Bash"})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "tool-err",
		ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	completeMeta, _ := json.Marshal(map[string]any{"is_error": true})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "tool-err",
		Meta: completeMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	items := findItemsByKind(t, st, "t1", itemKindToolCall)
	if len(items) != 1 || items[0].Status != statusErrored {
		t.Fatalf("expected status=errored, got %+v", items)
	}
}

// TestToolCompleteOnBackgroundedKeepsLaunchRunning pins the post-refactor
// contract: a backgrounded tool_result placeholder must NOT mutate the
// launch row's status (invariant: launch stays running until the actual
// task_lifecycle terminal arrives) and must NOT create a sibling
// completion row. The sibling is the responsibility of
// handleBackgroundTaskTerminal — see
// TestHandleEventBackgroundTaskTerminal_InsertsSibling for the
// sibling-row assertions.
func TestToolCompleteOnBackgroundedKeepsLaunchRunning(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	startMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Bash",
		"is_background": true,
		"input":         map[string]any{"command": "long &"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-tool",
		ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	exit := 0
	completeMeta, _ := json.Marshal(map[string]any{
		"is_background": true,
		"exit_code":     exit,
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "bg-tool",
		Meta: completeMeta, Content: "background result body", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	launches := findItemsByKind(t, st, "t1", itemKindToolCall)
	if len(launches) != 1 {
		t.Fatalf("expected launch row to remain, got %d tool_call items", len(launches))
	}
	if launches[0].Status != statusRunning {
		t.Errorf("background launch status = %q, want running (frozen)", launches[0].Status)
	}
	if !launches[0].IsBackground {
		t.Error("launch IsBackground lost")
	}

	// The placeholder tool_result must NOT materialize a sibling.
	// That's EventBackgroundTaskTerminal's job now.
	dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(dones) != 0 {
		t.Fatalf("placeholder tool_result must not create sibling; got %d background_done rows", len(dones))
	}
}

// TestToolCompleteOnBackgroundedPromotesMissingFlag covers the
// rescue case where EventToolStart missed the is_background flag (a
// provider drift we haven't observed but the code has always
// tolerated): the complete event carries is_background=true, so the
// launch row must flip to is_background=true without touching its
// status.
func TestToolCompleteOnBackgroundedPromotesMissingFlag(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Start without is_background — simulate a provider drift.
	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "Bash",
		"input":    map[string]any{"command": "long &"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "promote-tool",
		ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	completeMeta, _ := json.Marshal(map[string]any{"is_background": true})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "promote-tool",
		Meta: completeMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	launches := findItemsByKind(t, st, "t1", itemKindToolCall)
	if len(launches) != 1 {
		t.Fatalf("expected 1 launch row, got %d", len(launches))
	}
	if !launches[0].IsBackground {
		t.Error("expected is_background promoted by complete-event meta")
	}
	if launches[0].Status != statusRunning {
		t.Errorf("launch status = %q, want still running after promotion", launches[0].Status)
	}
}

// TestToolCompleteWithNoLaunchIsNoop guards against orphan completions
// (e.g. a partial replay that lost the start). We tolerate missing
// launches silently — failing here would mean a bad provider stream
// could corrupt the timeline.
func TestToolCompleteWithNoLaunchIsNoop(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "ghost",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("orphan completion produced items: %+v", items)
	}
}

// TestToolStartWithoutItemIDIsNoop guards the empty-id case. The Codex
// stream occasionally emits item events without an id (uncertain wire
// shape on certain notification kinds); persisting with id="" would
// collide with anything else missing an id, so we skip the row entirely
// rather than fabricate one.
func TestToolStartWithoutItemIDIsNoop(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	items := findItemsByKind(t, st, "t1", itemKindToolCall)
	if len(items) != 0 {
		t.Errorf("expected no item for empty id, got %+v", items)
	}
}

// TestToolStartIdempotentOnReplay covers duplicate EventToolStart
// delivery — e.g. Claude's resynthesised system/task_started after a
// reconnect, which lands a second EventToolStart with the same ItemID.
// The second start must not collide on the UNIQUE id constraint;
// instead the row's summary refreshes and is_background can flip if
// the classifier promoted the tool to background mid-stream. Status
// stays as-is so a stray late EventToolStart doesn't roll a completed
// row back to running.
func TestToolStartIdempotentOnReplay(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	startMeta1, _ := json.Marshal(map[string]any{
		"toolName": "Bash",
		"input":    map[string]any{"command": "pending command"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "rep-1",
		ItemType: "Bash", Meta: startMeta1, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("first start: %v", err)
	}

	// A re-delivery of EventToolStart arrives with refined input +
	// classifier promotion (e.g. reconnect-driven resynthesis).
	startMeta2, _ := json.Marshal(map[string]any{
		"toolName":      "Bash",
		"input":         map[string]any{"command": "pending command --verbose"},
		"is_background": true,
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "rep-1",
		ItemType: "Bash", Meta: startMeta2, Replace: true, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("replay start: %v", err)
	}

	items := findItemsByKind(t, st, "t1", itemKindToolCall)
	if len(items) != 1 {
		t.Fatalf("idempotent replay produced extra rows: %d", len(items))
	}
	if !strings.Contains(items[0].Summary, "verbose") {
		t.Errorf("summary not refreshed; got %q", items[0].Summary)
	}
	if !items[0].IsBackground {
		t.Errorf("classifier promotion not picked up; IsBackground=false")
	}
	if items[0].Status != statusRunning {
		t.Errorf("status mutated by replay: got %q, want running", items[0].Status)
	}
}

// TestInlineCompletionPreservesRichPayload guards against regression of
// review bug B1: when handleToolComplete runs, persistFileChangeToolResult
// links a tool_result payload onto the lifecycle row first, then
// persistToolCallCompletion flips status. The status flip must NOT NULL
// out the freshly-attached payload_id.
func TestInlineCompletionPreservesRichPayload(t *testing.T) {
	router, st, _ := newTestRouter(t)
	workspace := t.TempDir()
	createToolResultThread(t, st, "t1", workspace)

	startMeta := json.RawMessage(`{
		"toolName": "Edit",
		"item": {
			"id": "fc-rich",
			"type": "file_change",
			"title": "File change",
			"detail": "Editing src/foo.ts",
			"data": {
				"item": {
					"changes": [
						{"path": "src/foo.ts", "kind": {"type": "update", "move_path": null}}
					]
				}
			}
		}
	}`)
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "fc-rich",
		ItemType: "file_change", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	preCompletion, _, _ := st.GetItem("fc-rich")
	if preCompletion.PayloadID == "" {
		t.Fatalf("file_change start failed to attach payload — test setup invalid")
	}
	originalPayloadID := preCompletion.PayloadID

	// Now complete. The lifecycle status flip must NOT clear PayloadID.
	completeMeta := json.RawMessage(`{
		"item": {
			"id": "fc-rich",
			"type": "file_change",
			"title": "File change",
			"data": {
				"item": {
					"changes": [
						{"path": "src/foo.ts", "kind": {"type": "update", "move_path": null}}
					]
				}
			}
		}
	}`)
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "fc-rich",
		ItemType: "file_change", Meta: completeMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	postCompletion, _, _ := st.GetItem("fc-rich")
	if postCompletion.PayloadID == "" {
		t.Fatalf("payload wiped on completion (B1 regression)")
	}
	if postCompletion.PayloadID != originalPayloadID {
		t.Errorf("payload id changed: %q → %q", originalPayloadID, postCompletion.PayloadID)
	}
	if postCompletion.Status != statusCompleted {
		t.Errorf("status not flipped: %q", postCompletion.Status)
	}
}

// TestInlineCompletionAttachesPayloadWhenNoneExists covers the
// generic-tool path: a Bash invocation with no file_change side-effect
// arrives with a stdout body in evt.Content. The lifecycle completion
// stores that body as a tool_call_result payload so the dropdown can
// render it on demand.
func TestInlineCompletionAttachesPayloadWhenNoneExists(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	startMeta, _ := json.Marshal(map[string]any{"toolName": "Bash"})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bash-1",
		ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	completeMeta, _ := json.Marshal(map[string]any{"exit_code": 0})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "bash-1",
		Meta: completeMeta, Content: "total 0\ndrwx ...\n", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	item, _, _ := st.GetItem("bash-1")
	if item.PayloadID == "" {
		t.Fatalf("expected tool_call_result payload attached to inline completion")
	}
	data, err := st.GetPayloadData(item.PayloadID)
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if !strings.Contains(string(data), "total 0") {
		t.Errorf("payload data lost stdout body: %q", string(data))
	}
}

func TestToolCompletionWithoutContentDoesNotAttachEmptyPayload(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	startMeta, _ := json.Marshal(map[string]any{"toolName": "WebSearch"})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "web-1",
		ItemType: "webSearch", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	completeMeta, _ := json.Marshal(map[string]any{
		"toolName":    "WebSearch",
		"item_status": "completed",
		"input":       map[string]any{"query": "codex app-server webSearch"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "web-1",
		ItemType: "webSearch", Meta: completeMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	item, _, _ := st.GetItem("web-1")
	if item.PayloadID != "" {
		t.Fatalf("metadata-only completion attached empty payload %q", item.PayloadID)
	}
	if item.Summary != "WebSearch: codex app-server webSearch" {
		t.Fatalf("summary = %q, want final web query", item.Summary)
	}
}

func TestToolCompletionMergesCodexWaitAgentMeta(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	if err := st.UpdateProvider("t1", "codex"); err != nil {
		t.Fatalf("set provider: %v", err)
	}

	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "wait_agent",
		"input": map[string]any{
			"tool":              "wait_agent",
			"receiverThreadIds": []string{"child-1", "child-2"},
		},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "wait-1",
		ItemType: "wait_agent", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	completeMeta, _ := json.Marshal(map[string]any{
		"toolName":    "wait_agent",
		"item_status": "completed",
		"input": map[string]any{
			"tool":              "wait_agent",
			"receiverThreadIds": []string{"child-1", "child-2"},
			"agentsStates": map[string]any{
				"child-1": map[string]any{"status": "completed"},
				"child-2": map[string]any{"status": "completed"},
			},
		},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "wait-1",
		ItemType: "wait_agent", Meta: completeMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	item, _, _ := st.GetItem("wait-1")
	var meta map[string]any
	if err := json.Unmarshal([]byte(item.Meta), &meta); err != nil {
		t.Fatalf("parse item meta: %v", err)
	}
	input, ok := meta["input"].(map[string]any)
	if !ok {
		t.Fatalf("input missing: %#v", meta)
	}
	receivers, ok := input["receiverThreadIds"].([]any)
	if !ok || len(receivers) != 2 {
		t.Fatalf("receiverThreadIds = %#v, want two ids", input["receiverThreadIds"])
	}
}

func TestToolCompletionPreservesCodexWaitStartReceiversOnReplay(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	if err := st.UpdateProvider("t1", "codex"); err != nil {
		t.Fatalf("set provider: %v", err)
	}

	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "wait_agent",
		"input": map[string]any{
			"tool":              "wait_agent",
			"receiverThreadIds": []string{"child-1", "child-2", "child-3"},
		},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "wait-1",
		ItemType: "wait_agent", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	completeMeta, _ := json.Marshal(map[string]any{
		"toolName":    "wait_agent",
		"item_status": "completed",
		"input": map[string]any{
			"tool":              "wait_agent",
			"receiverThreadIds": []string{"child-1"},
			"agentsStates": map[string]any{
				"child-1": map[string]any{"status": "completed"},
			},
		},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "wait-1",
		ItemType: "wait_agent", Meta: completeMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	item, _, _ := st.GetItem("wait-1")
	var meta map[string]any
	if err := json.Unmarshal([]byte(item.Meta), &meta); err != nil {
		t.Fatalf("parse item meta: %v", err)
	}
	input, ok := meta["input"].(map[string]any)
	if !ok {
		t.Fatalf("input missing: %#v", meta)
	}
	receivers, ok := input["receiverThreadIds"].([]any)
	if !ok || len(receivers) != 3 {
		t.Fatalf("receiverThreadIds = %#v, want three start ids", input["receiverThreadIds"])
	}
	agentsStates, ok := input["agentsStates"].(map[string]any)
	if !ok || len(agentsStates) != 1 {
		t.Fatalf("agentsStates = %#v, want only completed child state", input["agentsStates"])
	}
}

func TestMcpCompletionContentAttachesPayload(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "MCP/lookup",
		"input":    map[string]any{"description": "docs/lookup"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "mcp-1",
		ItemType: "mcpToolCall", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	completeMeta, _ := json.Marshal(map[string]any{"item_status": "completed"})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "mcp-1",
		ItemType: "mcpToolCall", Meta: completeMeta, Content: "Lookup result", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	item, _, _ := st.GetItem("mcp-1")
	if item.PayloadID == "" {
		t.Fatal("expected MCP result payload")
	}
	data, err := st.GetPayloadData(item.PayloadID)
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if string(data) != "Lookup result" {
		t.Fatalf("payload = %q, want MCP result content", string(data))
	}
}

// TestToolStartFileChangeReusesIDForRichPayload covers the integration
// with the legacy tool_result helpers: when a file_change start arrives,
// persistToolCallLaunch creates the lifecycle row first; the existing
// persistFileChangeToolResult then attaches a tool_result payload to
// THAT row via UpdateItemPayload (no second item appended). Result: one
// row per file-change tool call, with both a status field and the rich
// inline-diff payload.
func TestToolStartFileChangeReusesIDForRichPayload(t *testing.T) {
	router, st, _ := newTestRouter(t)
	workspace := t.TempDir()
	createToolResultThread(t, st, "t1", workspace)

	meta := json.RawMessage(`{
		"toolName": "Edit",
		"item": {
			"id": "fc-1",
			"type": "file_change",
			"title": "File change",
			"detail": "Editing src/app.ts",
			"data": {
				"item": {
					"changes": [
						{
							"path": "src/app.ts",
							"kind": {"type": "update", "move_path": null}
						}
					]
				}
			}
		}
	}`)

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "fc-1",
		ItemType: "file_change", Meta: meta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	all, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected exactly one row for file_change start, got %d: %+v", len(all), all)
	}
	if all[0].Kind != itemKindToolCall {
		t.Errorf("kind = %q, want tool_call (lifecycle row owns the id)", all[0].Kind)
	}
	if all[0].PayloadID == "" {
		t.Error("expected tool_result payload attached, got empty PayloadID")
	}
}

// TestTaskStartedMergesTaskIDIntoItemMeta (Bug A) verifies that the
// Claude adapter's `system/task_started` event — an EventToolStart
// carrying ONLY task_id in Meta — is treated as a meta-merge onto an
// existing tool_call row. The launch row's summary / tool_name /
// status are preserved; only items.meta.task_id is updated.
func TestTaskStartedMergesTaskIDIntoItemMeta(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Real tool_use block: Bash background command.
	startMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Bash",
		"input":         map[string]any{"command": "pnpm run dev"},
		"is_background": true,
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "tool-bg-meta",
		ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}

	// task_started arrives with only task_id in meta.
	taskStartedMeta, _ := json.Marshal(map[string]any{"task_id": "task-xyz"})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "tool-bg-meta",
		Meta: taskStartedMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("task_started meta update: %v", err)
	}

	item, _, err := st.GetItem("tool-bg-meta")
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if item.Status != statusRunning {
		t.Errorf("status = %q, want running (meta merge must not stall lifecycle)", item.Status)
	}
	if !item.IsBackground {
		t.Error("IsBackground lost during meta merge")
	}
	if !strings.Contains(item.Summary, "Bash") || !strings.Contains(item.Summary, "pnpm run dev") {
		t.Errorf("summary overwritten by meta merge: %q", item.Summary)
	}

	var persistedMeta map[string]any
	if err := json.Unmarshal([]byte(item.Meta), &persistedMeta); err != nil {
		t.Fatalf("unmarshal persisted meta: %v", err)
	}
	if persistedMeta["task_id"] != "task-xyz" {
		t.Errorf("items.meta.task_id = %v, want task-xyz", persistedMeta["task_id"])
	}
}

// TestSubagentModelMergesIntoItemMetaWithoutClobber verifies that the
// claude parser's per-subagent model meta-update — an EventToolStart
// carrying ONLY `subagent_model` in Meta — merges onto the parent
// Agent tool_call row. The launch row's summary / tool_name / status
// are preserved; only items.meta.subagent_model is added. This is the
// signal the SubagentGroup card reads to render `<agent_type>
// (<Model>)` in the header.
func TestSubagentModelMergesIntoItemMetaWithoutClobber(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "Agent",
		"input": map[string]any{
			"description":   "Find foo",
			"subagent_type": "Explore",
		},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "agent-tool-1",
		ItemType: "Agent", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}

	subagentModelMeta, _ := json.Marshal(map[string]any{"subagent_model": "claude-opus-4-7"})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "agent-tool-1",
		Meta: subagentModelMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("subagent_model meta update: %v", err)
	}

	item, _, err := st.GetItem("agent-tool-1")
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if item.Status != statusRunning {
		t.Errorf("status = %q, want running (meta merge must not stall lifecycle)", item.Status)
	}
	if item.ToolName != "Agent" {
		t.Errorf("ToolName overwritten by meta merge: %q", item.ToolName)
	}
	if !strings.Contains(item.Summary, "Agent") || !strings.Contains(item.Summary, "Find foo") {
		t.Errorf("summary overwritten by meta merge: %q", item.Summary)
	}
	var persistedMeta map[string]any
	if err := json.Unmarshal([]byte(item.Meta), &persistedMeta); err != nil {
		t.Fatalf("unmarshal persisted meta: %v", err)
	}
	if persistedMeta["subagent_model"] != "claude-opus-4-7" {
		t.Errorf("items.meta.subagent_model = %v, want claude-opus-4-7", persistedMeta["subagent_model"])
	}

	// A redundant second meta update with the same value must be a
	// no-op (no new persisted_at bumps, same status).
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "agent-tool-1",
		Meta: subagentModelMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("redundant subagent_model meta update: %v", err)
	}
	item2, _, _ := st.GetItem("agent-tool-1")
	if item2.UpdatedAt != item.UpdatedAt {
		t.Errorf("redundant meta update bumped UpdatedAt: was=%d now=%d", item.UpdatedAt, item2.UpdatedAt)
	}
}

func TestCodexSpawnLabelMetaUpdatePreservesFullReceiverList(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "collab_agent",
		"input": map[string]any{
			"tool":              "spawn_agent",
			"prompt":            "Run sleep jobs",
			"receiverThreadIds": []string{"child-1", "child-2"},
		},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "spawn-1",
		ItemType: "collab_agent", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}

	updateMeta, _ := json.Marshal(map[string]any{
		"meta_update_only": true,
		"toolName":         "collab_agent",
		"input": map[string]any{
			"tool":              "spawn_agent",
			"receiverThreadIds": []string{"child-1", "child-2"},
			"newAgentNickname":  "Hypatia",
			"newAgentRole":      "default",
		},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "spawn-1",
		ItemType: "collab_agent", Meta: updateMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("meta update: %v", err)
	}

	item, _, err := st.GetItem("spawn-1")
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	var persistedMeta struct {
		Input struct {
			ReceiverThreadIDs []string `json:"receiverThreadIds"`
			NewAgentNickname  string   `json:"newAgentNickname"`
			NewAgentRole      string   `json:"newAgentRole"`
		} `json:"input"`
	}
	if err := json.Unmarshal([]byte(item.Meta), &persistedMeta); err != nil {
		t.Fatalf("unmarshal persisted meta: %v", err)
	}
	want := []string{"child-1", "child-2"}
	if !reflect.DeepEqual(persistedMeta.Input.ReceiverThreadIDs, want) {
		t.Fatalf("receiverThreadIds = %+v, want %+v", persistedMeta.Input.ReceiverThreadIDs, want)
	}
	if persistedMeta.Input.NewAgentNickname != "Hypatia" || persistedMeta.Input.NewAgentRole != "default" {
		t.Fatalf("agent label = %q/%q, want Hypatia/default", persistedMeta.Input.NewAgentNickname, persistedMeta.Input.NewAgentRole)
	}
}

// TestMergeItemMetaCorrelationFieldsRebuildsMalformedExisting verifies
// that a corrupt items.meta blob (e.g. truncated JSON from a prior
// crash) does not block the merge — the helper rebuilds the meta
// around the requested correlation fields rather than carrying the
// malformed payload forward. SQLite's CHECK(json_valid(meta))
// constraint guarantees we never see garbage on the integration path,
// but the helper has to be safe regardless because the parsed map is
// the only structured handle to existing fields. Tested directly
// because the persistence layer rejects malformed JSON at insert time.
func TestMergeItemMetaCorrelationFieldsRebuildsMalformedExisting(t *testing.T) {
	out, err := mergeItemMetaCorrelationFields(`{"toolName":"Agent","input":{`, itemMetaCorrelationFields{
		SubagentModel: "claude-opus-4-7",
	})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("rebuilt meta still malformed: %v (%s)", err, out)
	}
	if parsed["subagent_model"] != "claude-opus-4-7" {
		t.Errorf("subagent_model not on rebuilt meta: %v", parsed["subagent_model"])
	}
	// The malformed fragment is dropped on rebuild — no carry-forward.
	if _, lingering := parsed["toolName"]; lingering {
		t.Errorf("malformed meta carried partial fields forward: %v", parsed)
	}
}

// TestTaskStartedMetaMergeNoopsWhenItemMissing covers the reconnect
// edge case where task_started fires for a tool_use that was never
// observed (crash-replay lost the launch). Triage drops the meta-only
// event rather than fabricating a ghost tool_call row.
func TestTaskStartedMetaMergeNoopsWhenItemMissing(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	taskStartedMeta, _ := json.Marshal(map[string]any{"task_id": "orphan-task"})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "ghost-tool",
		Meta: taskStartedMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("orphan meta merge: %v", err)
	}
	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected no rows for orphan meta merge, got %+v", items)
	}
}

// TestTaskUpdatedResolvesItemViaMetaTaskID (Bug A reconnect-recovery
// scenario) verifies that when a fresh adapter session emits a
// task_updated EventBackgroundTaskTerminal with empty tool_use_id
// and only task_id, triage looks up the matching tool_call row via
// items.meta.task_id and stashes the terminal keyed by both task_id
// and the resolved tool_use_id.
//
// Post-stash refactor: task_updated NEVER writes the sibling — it
// stashes the terminal in pending_background_task_terminals so the
// tray query hides the launch. The sibling is written later when
// the agent observes completion via TaskOutput or
// task_notification. See
// docs/architecture/turn-lifecycle.md §Task lifecycle.
func TestTaskUpdatedResolvesItemViaMetaTaskID(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Seed the tool_call row (pre-reconnect adapter would have done this).
	startMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Bash",
		"input":         map[string]any{"command": "long-running"},
		"is_background": true,
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-recov",
		ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("seed tool_call: %v", err)
	}

	// Adapter persists task_id into items.meta via task_started.
	taskStartedMeta, _ := json.Marshal(map[string]any{"task_id": "recov-task"})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-recov",
		Meta: taskStartedMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("task_started meta merge: %v", err)
	}

	// Simulate app restart: a fresh adapter session parser has an
	// empty in-memory task_id map. task_updated on the live server
	// fires with only task_id inline, producing an
	// EventBackgroundTaskTerminal with empty ItemID + meta.task_id.
	terminalMeta, _ := json.Marshal(map[string]any{
		"task_id": "recov-task",
		"status":  "completed",
		"source":  "task_updated",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "",
		Meta: terminalMeta, Content: "terminated", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("task_updated terminal: %v", err)
	}

	// Launch stays as the only chat row; sibling NOT yet written
	// (agent has not observed completion).
	launches := findItemsByKind(t, st, "t1", itemKindToolCall)
	if len(launches) != 1 {
		t.Fatalf("expected 1 tool_call, got %+v", launches)
	}
	if launches[0].ID != "bg-recov" {
		t.Errorf("unexpected launch id %q", launches[0].ID)
	}
	dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(dones) != 0 {
		t.Fatalf("task_updated should not write a sibling; got %+v", dones)
	}

	// Stash row exists, keyed by (thread_id, task_id), with
	// tool_use_id resolved from the items.meta.task_id index.
	stash, found, err := st.GetPendingBackgroundTerminal("t1", "recov-task")
	if err != nil {
		t.Fatalf("read stash: %v", err)
	}
	if !found {
		t.Fatal("expected stash row for recov-task; reconnect resolution didn't write one")
	}
	if stash.ToolUseID != "bg-recov" {
		t.Errorf("stash.ToolUseID = %q, want bg-recov (lookup via meta.task_id failed)", stash.ToolUseID)
	}
	if stash.Status != "completed" {
		t.Errorf("stash.Status = %q, want completed", stash.Status)
	}
	if stash.Source != "task_updated" {
		t.Errorf("stash.Source = %q, want task_updated", stash.Source)
	}
}

// TestTaskUpdatedUnmatchedMetaTaskIDIsDropped (Bug A edge case) covers
// the "log and drop" branch: adapter emits
// EventBackgroundTaskTerminal keyed by meta.task_id but no tool_call
// row carries that task_id — the launch was never persisted. Triage
// must not error and must not create a phantom completion row.
func TestTaskUpdatedUnmatchedMetaTaskIDIsDropped(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	terminalMeta, _ := json.Marshal(map[string]any{
		"task_id": "missing-task",
		"status":  "completed",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "",
		Meta: terminalMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("unmatched task_id terminal: %v", err)
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected no rows for unmatched meta.task_id, got %+v", items)
	}
}

func TestKilledTaskUpdatedWithoutLaunchDoesNotCreateOrphanCompletion(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	terminalMeta, _ := json.Marshal(map[string]any{
		"task_id":     "missing-killed-task",
		"tool_use_id": "hidden-child-tool",
		"status":      "killed",
		"is_error":    true,
		"source":      "task_updated",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "hidden-child-tool",
		Meta: terminalMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("unmatched killed terminal: %v", err)
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected no orphan rows for hidden killed task, got %+v", items)
	}
	if _, found, err := st.GetPendingBackgroundTerminal("t1", "missing-killed-task"); err != nil {
		t.Fatalf("read stash: %v", err)
	} else if found {
		t.Fatal("killed task_updated should not leave a stash")
	}
}

func TestTaskOutputWithoutLaunchDoesNotCreateOrphanCompletion(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	terminalMeta, _ := json.Marshal(map[string]any{
		"task_id":     "missing-output-task",
		"tool_use_id": "hidden-child-tool",
		"status":      "failed",
		"is_error":    true,
		"source":      "task_output",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "hidden-child-tool",
		Meta: terminalMeta, Content: "tool failed", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("unmatched task_output terminal: %v", err)
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected no orphan rows for hidden task_output terminal, got %+v", items)
	}
}

// TestHandleEventBackgroundTaskTerminal_InsertsSibling pins the
// task-lifecycle observation contract. A backgrounded launch plus a
// task_output-sourced EventBackgroundTaskTerminal (the agent observed
// completion via TaskOutput tool_result) must produce exactly one
// tool_completion sibling keyed by the launch id, leaving the launch
// row frozen at running. The timeline renders "agent dispatched this
// background tool" and "the background work actually finished" as two
// historically accurate rows. Note: a task_updated-sourced terminal
// alone only stashes — see TestHandleEventBackgroundTaskTerminal_TaskUpdatedStashesNoSibling.
func TestHandleEventBackgroundTaskTerminal_InsertsSibling(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	startMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Bash",
		"is_background": true,
		"input":         map[string]any{"command": "sleep 30"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-insert",
		ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("seed launch: %v", err)
	}

	exit := 0
	terminalMeta, _ := json.Marshal(map[string]any{
		"task_id":     "tsk-insert",
		"tool_use_id": "bg-insert",
		"status":      "completed",
		"exit_code":   exit,
		"source":      "task_output",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "bg-insert",
		Meta: terminalMeta, Content: "done body", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("bg terminal: %v", err)
	}

	launches := findItemsByKind(t, st, "t1", itemKindToolCall)
	if len(launches) != 1 {
		t.Fatalf("expected 1 tool_call launch, got %d", len(launches))
	}
	if launches[0].Status != statusRunning {
		t.Errorf("launch status = %q, want %q (frozen per background lifecycle)", launches[0].Status, statusRunning)
	}
	if !launches[0].IsBackground {
		t.Error("launch IsBackground lost")
	}

	dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(dones) != 1 {
		t.Fatalf("expected exactly 1 tool_completion sibling, got %d", len(dones))
	}
	done := dones[0]
	if done.ID != nextToolCompletionID("bg-insert") {
		t.Errorf("sibling id = %q, want %q", done.ID, nextToolCompletionID("bg-insert"))
	}
	if done.CompletionOf != "bg-insert" {
		t.Errorf("CompletionOf = %q, want bg-insert", done.CompletionOf)
	}
	if !done.IsBackground {
		t.Error("sibling IsBackground = false")
	}
	if done.Status != statusCompleted {
		t.Errorf("sibling Status = %q, want completed", done.Status)
	}
	if done.PayloadID == "" {
		t.Error("expected sibling to carry a result payload")
	}

	// A second invocation with the same task_id must be idempotent — no
	// new row; the existing sibling is re-upserted in place.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "bg-insert",
		Meta: terminalMeta, Content: "done body", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("bg terminal (2nd): %v", err)
	}
	dones = findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(dones) != 1 {
		t.Fatalf("expected 1 tool_completion after duplicate terminal (idempotent upsert), got %d", len(dones))
	}

	// Sibling emission lands as provider:item_event with the same id
	// so the frontend reconciler merges in place.
	siblingUpserts := 0
	for _, item := range filterItemEventUpserts(*emissions) {
		if item.ID == nextToolCompletionID("bg-insert") {
			siblingUpserts++
		}
	}
	if siblingUpserts < 2 {
		t.Errorf("expected at least 2 sibling upsert emissions (insert + idempotent refresh), got %d", siblingUpserts)
	}
}

// TestHandleEventBackgroundTaskTerminal_TaskUpdatedStashesNoSibling
// pins the post-stash refactor invariant: source=task_updated is the
// HOST process exit signal, NOT the agent observation. It must
// stash a pending_background_task_terminals row (so the tray hides
// the launch) but MUST NOT write the chat-side `tool_completion`
// sibling — the agent has not yet seen the completion.
//
// The chat row is only written later when the agent observes via
// TaskOutput (source=task_output) or task_notification.
func TestHandleEventBackgroundTaskTerminal_TaskUpdatedStashesNoSibling(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	startMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Bash",
		"is_background": true,
		"input":         map[string]any{"command": "sleep 30"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-stash",
		ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("seed launch: %v", err)
	}

	exit := 0
	terminalMeta, _ := json.Marshal(map[string]any{
		"task_id":     "tsk-stash",
		"tool_use_id": "bg-stash",
		"status":      "completed",
		"exit_code":   exit,
		"source":      "task_updated",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "bg-stash",
		Meta: terminalMeta, Content: "ignored body", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("bg terminal: %v", err)
	}

	// No sibling: agent has not observed completion.
	dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(dones) != 0 {
		t.Fatalf("task_updated must not write a sibling, got %+v", dones)
	}

	// Launch row is still the only chat row, frozen at running.
	launches := findItemsByKind(t, st, "t1", itemKindToolCall)
	if len(launches) != 1 || launches[0].Status != statusRunning {
		t.Fatalf("launch missing or not running: %+v", launches)
	}

	// Stash row exists keyed by (thread_id, task_id).
	stash, found, err := st.GetPendingBackgroundTerminal("t1", "tsk-stash")
	if err != nil {
		t.Fatalf("read stash: %v", err)
	}
	if !found {
		t.Fatal("expected stash row, none persisted")
	}
	if stash.ToolUseID != "bg-stash" {
		t.Errorf("stash.ToolUseID = %q, want bg-stash", stash.ToolUseID)
	}
	if stash.Status != "completed" {
		t.Errorf("stash.Status = %q, want completed", stash.Status)
	}
	if stash.ExitCode == nil || *stash.ExitCode != 0 {
		t.Errorf("stash.ExitCode = %v, want 0", stash.ExitCode)
	}

	// Tray query must hide the launch (stash NOT EXISTS predicate).
	live, err := st.ListLiveBackgroundTasks("t1", 0)
	if err != nil {
		t.Fatalf("list live: %v", err)
	}
	for _, row := range live {
		if row.ID == "bg-stash" {
			t.Fatalf("tray still shows launch after task_updated stash: %+v", row)
		}
	}

	// Frontend nudge: provider:background_task_state with state=exited.
	sawExited := false
	for _, e := range *emissions {
		if e.eventName != "provider:background_task_state" {
			continue
		}
		payload, ok := e.data.(BackgroundTaskStateEvent)
		if !ok {
			continue
		}
		if payload.TaskID == "tsk-stash" && payload.State == "exited" {
			sawExited = true
			if payload.LaunchID != "bg-stash" {
				t.Errorf("background_task_state.LaunchID = %q, want bg-stash", payload.LaunchID)
			}
		}
	}
	if !sawExited {
		t.Error("expected provider:background_task_state(state=exited) emission")
	}
}

// TestHandleEventBackgroundTaskTerminal_StashThenObservationFlow
// covers the canonical two-event flow: task_updated stashes the
// host-side exit, then a later task_output enrichment drains the
// stash and writes the sibling carrying the merged data
// (status/exit_code from stash, output_file from observation).
func TestHandleEventBackgroundTaskTerminal_StashThenObservationFlow(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	startMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Bash",
		"is_background": true,
		"input":         map[string]any{"command": "long job"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-flow",
		ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("seed launch: %v", err)
	}

	// Phase 1: task_updated. Stash, no sibling.
	exit := 0
	stashMeta, _ := json.Marshal(map[string]any{
		"task_id":     "tsk-flow",
		"tool_use_id": "bg-flow",
		"status":      "completed",
		"exit_code":   exit,
		"end_time":    int64(1700000000000),
		"source":      "task_updated",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "bg-flow",
		Meta: stashMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("stash phase: %v", err)
	}
	if dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone); len(dones) != 0 {
		t.Fatalf("phase 1 should not write a sibling, got %+v", dones)
	}

	// Phase 2: task_output observation arrives. Different (richer)
	// fields — output_file present here, status/exit_code already in
	// stash. Drains stash, writes sibling, emits state=drained.
	*emissions = (*emissions)[:0]
	observeMeta, _ := json.Marshal(map[string]any{
		"task_id":     "tsk-flow",
		"tool_use_id": "bg-flow",
		"output_file": "/tmp/long-job.txt",
		"source":      "task_output",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "bg-flow",
		Meta: observeMeta, Content: "stdout body", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("observe phase: %v", err)
	}

	dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(dones) != 1 {
		t.Fatalf("expected 1 sibling after observation drain, got %+v", dones)
	}
	done := dones[0]
	if done.CompletionOf != "bg-flow" {
		t.Errorf("CompletionOf = %q, want bg-flow", done.CompletionOf)
	}
	if done.Status != statusCompleted {
		t.Errorf("Status = %q, want completed (merged from stash)", done.Status)
	}
	doneMeta := decodeItemMetaMap(t, done.Meta)
	if doneMeta["output_file"] != "/tmp/long-job.txt" {
		t.Errorf("output_file = %v, want /tmp/long-job.txt (from observation)", doneMeta["output_file"])
	}

	// Stash drained — no longer present.
	if _, found, err := st.GetPendingBackgroundTerminal("t1", "tsk-flow"); err != nil {
		t.Fatalf("read stash post-drain: %v", err)
	} else if found {
		t.Error("stash row not drained after observation")
	}

	// Frontend nudge: provider:background_task_state(state=drained).
	sawDrained := false
	for _, e := range *emissions {
		if e.eventName != "provider:background_task_state" {
			continue
		}
		payload, ok := e.data.(BackgroundTaskStateEvent)
		if !ok {
			continue
		}
		if payload.TaskID == "tsk-flow" && payload.State == "drained" {
			sawDrained = true
		}
	}
	if !sawDrained {
		t.Error("expected provider:background_task_state(state=drained) emission")
	}
}

// TestHandleEventBackgroundTaskTerminal_AppendsAtCurrentTurn pins the
// history model for background work: the launch row stays where the
// agent dispatched it, but the terminal row lands where completion was
// observed. A background Bash can finish several turns later, and the
// chat timeline must show that as a separate later event.
func TestHandleEventBackgroundTaskTerminal_AppendsAtCurrentTurn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Bash",
		"is_background": true,
		"input":         map[string]any{"command": "sleep 30"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-late",
		ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("seed launch: %v", err)
	}

	// Simulate later chat activity: turn 2 is now the active write
	// head when the background task reports terminal.
	seedOpenTurn(t, router, st, "t1", 2)
	terminalMeta, _ := json.Marshal(map[string]any{
		"task_id":     "tsk-late",
		"tool_use_id": "bg-late",
		"status":      "completed",
		"source":      "task_output",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "bg-late",
		Meta: terminalMeta, Content: "done", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("bg terminal: %v", err)
	}

	launch, ok, err := st.GetThreadItem("t1", "bg-late")
	if err != nil || !ok {
		t.Fatalf("lookup launch: ok=%v err=%v", ok, err)
	}
	if launch.TurnIndex != 0 {
		t.Fatalf("launch turn_index = %d, want 0", launch.TurnIndex)
	}

	dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(dones) != 1 {
		t.Fatalf("expected 1 tool_completion sibling, got %d", len(dones))
	}
	if dones[0].TurnIndex != 2 {
		t.Errorf("completion turn_index = %d, want 2 (current write head)", dones[0].TurnIndex)
	}
	if dones[0].CompletionOf != "bg-late" {
		t.Errorf("completion_of = %q, want bg-late", dones[0].CompletionOf)
	}
}

// TestHandleEventBackgroundTaskTerminal_AppendsToLatestPersistedTurn
// covers the no-open-turn fallback. The turns table can legitimately
// know about a later turn even when that turn produced no items; the
// completion should still land at the latest recorded history position,
// not back on the launch turn.
func TestHandleEventBackgroundTaskTerminal_AppendsToLatestPersistedTurn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Bash",
		"is_background": true,
		"input":         map[string]any{"command": "sleep 30"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-empty-turn",
		ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("seed launch: %v", err)
	}
	router.clearOpenTurn("t1")

	if err := st.InsertTurn(store.Turn{
		TurnID:    "turn-3",
		ThreadID:  "t1",
		TurnIndex: 3,
		StartedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("insert itemless later turn: %v", err)
	}

	terminalMeta, _ := json.Marshal(map[string]any{
		"task_id":     "tsk-empty-turn",
		"tool_use_id": "bg-empty-turn",
		"status":      "completed",
		"source":      "task_output",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "bg-empty-turn",
		Meta: terminalMeta, Content: "done", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("bg terminal: %v", err)
	}

	dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(dones) != 1 {
		t.Fatalf("expected 1 tool_completion sibling, got %d", len(dones))
	}
	if dones[0].TurnIndex != 3 {
		t.Errorf("completion turn_index = %d, want latest persisted turn 3", dones[0].TurnIndex)
	}
}

// TestHandleEventBackgroundTaskTerminal_Enriches covers the two-event
// task-updated + TaskOutput sequence where the second call carries a
// richer payload (exit_code, output_file) than the first. The sibling
// row must upsert in place — same id, no duplicate row — and the
// richer payload must win.
func TestHandleEventBackgroundTaskTerminal_Enriches(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	startMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Bash",
		"is_background": true,
		"input":         map[string]any{"command": "sleep 5; echo done"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-enrich",
		ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("seed launch: %v", err)
	}

	// First terminal from TaskOutput — basic shape (no exit_code /
	// output_file yet). Note: task_updated alone wouldn't create a
	// sibling under the post-stash model — see
	// TestHandleEventBackgroundTaskTerminal_TaskUpdatedStashesNoSibling.
	basicMeta, _ := json.Marshal(map[string]any{
		"task_id":     "tsk-enrich",
		"tool_use_id": "bg-enrich",
		"status":      "completed",
		"source":      "task_output",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "bg-enrich",
		Meta: basicMeta, Content: "", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("basic terminal: %v", err)
	}
	dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(dones) != 1 {
		t.Fatalf("expected 1 sibling after basic terminal, got %d", len(dones))
	}

	// Second terminal from TaskOutput — richer payload.
	exit := 0
	enrichedMeta, _ := json.Marshal(map[string]any{
		"task_id":     "tsk-enrich",
		"tool_use_id": "bg-enrich",
		"status":      "completed",
		"exit_code":   exit,
		"output_file": "/tmp/bg-output.txt",
		"source":      "task_output",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "bg-enrich",
		Meta: enrichedMeta, Content: "stdout body here", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("enriched terminal: %v", err)
	}

	donesAfter := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(donesAfter) != 1 {
		t.Fatalf("expected still 1 sibling after enrichment, got %d (enrichment created a duplicate)", len(donesAfter))
	}
	done := donesAfter[0]
	if done.PayloadID == "" {
		t.Fatal("expected enriched sibling to carry a payload")
	}
	meta, err := st.GetPayloadMeta(done.PayloadID)
	if err != nil {
		t.Fatalf("read enriched payload meta: %v", err)
	}
	if !strings.Contains(meta.Meta, "outputFile") {
		t.Errorf("expected enriched meta to carry outputFile, got %q", meta.Meta)
	}
	data, err := st.GetPayloadData(done.PayloadID)
	if err != nil {
		t.Fatalf("read enriched payload data: %v", err)
	}
	if string(data) != "stdout body here" {
		t.Errorf("expected enriched payload data to be replaced, got %q", string(data))
	}
}

func TestHandleEventBackgroundTaskTerminal_EnrichmentPreservesCompletionTurn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Bash",
		"is_background": true,
		"input":         map[string]any{"command": "sleep 5; echo done"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-stable-turn",
		ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("seed launch: %v", err)
	}

	basicMeta, _ := json.Marshal(map[string]any{
		"task_id":     "tsk-stable-turn",
		"tool_use_id": "bg-stable-turn",
		"status":      "completed",
		"source":      "task_output",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "bg-stable-turn",
		Meta: basicMeta, Content: "", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("basic terminal: %v", err)
	}
	initial, ok, err := st.GetThreadItem("t1", nextToolCompletionID("bg-stable-turn"))
	if err != nil || !ok {
		t.Fatalf("lookup initial sibling: ok=%v err=%v", ok, err)
	}
	if initial.TurnIndex != 0 {
		t.Fatalf("initial turn_index=%d, want 0", initial.TurnIndex)
	}

	seedOpenTurn(t, router, st, "t1", 2)
	exit := 0
	enrichedMeta, _ := json.Marshal(map[string]any{
		"task_id":     "tsk-stable-turn",
		"tool_use_id": "bg-stable-turn",
		"status":      "completed",
		"exit_code":   exit,
		"output_file": "/tmp/bg-output.txt",
		"source":      "task_output",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "bg-stable-turn",
		Meta: enrichedMeta, Content: "stdout body here", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("enriched terminal: %v", err)
	}

	after, ok, err := st.GetThreadItem("t1", nextToolCompletionID("bg-stable-turn"))
	if err != nil || !ok {
		t.Fatalf("lookup enriched sibling: ok=%v err=%v", ok, err)
	}
	if after.TurnIndex != initial.TurnIndex {
		t.Fatalf("enrichment moved completion turn_index from %d to %d", initial.TurnIndex, after.TurnIndex)
	}
	if after.ItemIndex != initial.ItemIndex {
		t.Fatalf("enrichment moved item_index from %d to %d", initial.ItemIndex, after.ItemIndex)
	}
}

// TestHandleEventBackgroundTaskTerminal_ResolvesByTaskIDWhenToolUseMissing
// covers the reconnect path: a fresh adapter session has an empty
// task_id↔tool_use_id map, so the terminal event arrives with only
// meta.task_id (tool_use_id empty). Triage must look up the launch
// via items.meta.task_id.
func TestHandleEventBackgroundTaskTerminal_ResolvesByTaskIDWhenToolUseMissing(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	startMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Bash",
		"is_background": true,
		"input":         map[string]any{"command": "long job"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-resolve",
		ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("seed launch: %v", err)
	}

	// Adapter attaches task_id to the row via a meta-only tool_start.
	taskStartedMeta, _ := json.Marshal(map[string]any{"task_id": "tsk-resolve"})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-resolve",
		Meta: taskStartedMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("task_started meta merge: %v", err)
	}

	// Reconnect: terminal arrives with no tool_use_id. Carries
	// source=task_output (TaskOutput drains the stash), since
	// task_updated alone would only stash without writing the sibling.
	terminalMeta, _ := json.Marshal(map[string]any{
		"task_id": "tsk-resolve",
		"status":  "completed",
		"source":  "task_output",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "",
		Meta: terminalMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("terminal via task_id: %v", err)
	}

	dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(dones) != 1 {
		t.Fatalf("expected 1 sibling resolved via task_id lookup, got %d", len(dones))
	}
	if dones[0].CompletionOf != "bg-resolve" {
		t.Errorf("CompletionOf = %q, want bg-resolve", dones[0].CompletionOf)
	}
}

// TestHandleEventBackgroundTaskTerminal_DropsWhenNoLaunch guards the
// "launch never persisted" edge case. The terminal event is silently
// dropped — no phantom sibling row, no panic, no error.
func TestHandleEventBackgroundTaskTerminal_DropsWhenNoLaunch(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	terminalMeta, _ := json.Marshal(map[string]any{
		"task_id":     "tsk-phantom",
		"tool_use_id": "ghost-tool",
		"status":      "completed",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "ghost-tool",
		Meta: terminalMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("phantom terminal: %v", err)
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected no rows when launch is missing, got %+v", items)
	}
}

// TestRecoverOrphanedBackgroundTasks pins the startup-recovery
// contract: an orphaned background launch (running, no completion
// sibling, no stash entry) gets a synthesized session_died stash plus
// a `tool_completion` sibling so the tray clears and the chat row
// shows `killed`. Re-running the recovery is idempotent: the second
// pass sees the now-existing sibling and skips the launch.
func TestRecoverOrphanedBackgroundTasks(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Seed a backgrounded launch with task_id meta — mimics the live
	// flow where Claude task_started attached the task_id correlation.
	startMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Bash",
		"is_background": true,
		"input":         map[string]any{"command": "long-running"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-orphan",
		ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("seed launch: %v", err)
	}
	taskStartedMeta, _ := json.Marshal(map[string]any{"task_id": "tsk-orphan"})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-orphan",
		Meta: taskStartedMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("task_started meta merge: %v", err)
	}

	// Sanity: launch is the only row, no sibling yet.
	if dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone); len(dones) != 0 {
		t.Fatalf("precondition: no sibling expected, got %+v", dones)
	}

	recovered, err := router.RecoverOrphanedBackgroundTasks()
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("recovered = %d, want 1", recovered)
	}

	// Sibling materialised with status=killed.
	dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(dones) != 1 {
		t.Fatalf("expected 1 recovery sibling, got %+v", dones)
	}
	if dones[0].CompletionOf != "bg-orphan" {
		t.Errorf("CompletionOf = %q, want bg-orphan", dones[0].CompletionOf)
	}
	if dones[0].Status != statusKilled {
		t.Errorf("Status = %q, want %q", dones[0].Status, statusKilled)
	}

	// Recovery never writes a stash row — it goes straight to the
	// sibling, so a half-state crash leaves the launch re-discoverable
	// by the next sweep.
	if _, found, err := st.GetPendingBackgroundTerminal("t1", "tsk-orphan"); err != nil {
		t.Fatalf("read stash: %v", err)
	} else if found {
		t.Error("recovery should not create a stash row")
	}

	// Live tasks query returns the launch + its sibling joined so the
	// user sees "I started this, it ended killed" together until they
	// age out via retention. The launch alone (without sibling) is the
	// pre-recovery state that the stash predicate would have hidden.
	live, err := st.ListLiveBackgroundTasks("t1", 0)
	if err != nil {
		t.Fatalf("list live: %v", err)
	}
	var sawLaunch, sawSibling bool
	for _, row := range live {
		switch row.ID {
		case "bg-orphan":
			sawLaunch = true
		case backgroundCompletionID("bg-orphan", "tsk-orphan"):
			sawSibling = true
		}
	}
	if !sawLaunch {
		t.Error("expected launch in tray after recovery (joined with sibling)")
	}
	if !sawSibling {
		t.Error("expected recovery sibling in tray, joined to launch")
	}

	// Idempotent: second pass finds nothing to recover.
	recovered2, err := router.RecoverOrphanedBackgroundTasks()
	if err != nil {
		t.Fatalf("recover (2nd): %v", err)
	}
	if recovered2 != 0 {
		t.Errorf("idempotency broken: 2nd pass recovered %d rows, want 0", recovered2)
	}
	if dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone); len(dones) != 1 {
		t.Errorf("idempotency broken: sibling count after 2nd pass = %d, want 1", len(dones))
	}
}

// TestRecoverOrphanedBackgroundTasksSkipsLaunchWithoutTaskID pins the
// edge case for launches that never received their task_started meta
// merge before the previous app instance died. Without a task_id we
// have no idempotency key for the stash, so the row is logged and
// left alone — better than synthesising a stash keyed by the empty
// string and corrupting the index.
func TestRecoverOrphanedBackgroundTasksSkipsLaunchWithoutTaskID(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	startMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Bash",
		"is_background": true,
		"input":         map[string]any{"command": "race"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-no-task-id",
		ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("seed launch: %v", err)
	}

	recovered, err := router.RecoverOrphanedBackgroundTasks()
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if recovered != 0 {
		t.Errorf("recovered = %d, want 0 for no-task-id launch", recovered)
	}
	if dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone); len(dones) != 0 {
		t.Errorf("unexpected sibling for no-task-id launch: %+v", dones)
	}
}

// TestForceClosedRow_LateCompletionDoesNotResurrect pins spec
// invariant 23 (docs/architecture/turn-lifecycle.md §Force-close
// safety net): once the turn-complete handler has force-closed a
// running tool_call to `errored`, a late EventToolComplete is noise
// and must not resurrect the row. The row stays `errored`, the
// force-close marker survives, and no sibling tool_completion is
// written (invariant 5).
func TestForceClosedRow_LateCompletionDoesNotResurrect(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// 1. Turn 1 starts.
	startedAt := time.UnixMilli(1_700_000_000_000)
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1,
		Timestamp: startedAt,
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	// 2. Inline tool_call (not backgrounded) launches.
	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "Bash",
		"input":    map[string]any{"command": "whoami"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "late-complete",
		ItemType: "Bash", Meta: startMeta, Timestamp: startedAt.Add(100 * time.Millisecond),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}

	// 3. Turn completes without any matching EventToolComplete —
	// simulates a dropped tool_result. Force-close flips the row to
	// errored with the force-close marker.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		Timestamp: startedAt.Add(500 * time.Millisecond),
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}

	// Sanity: the force-close safety net ran and left the row errored
	// with the force-close marker — without this, the later
	// resurrection assertion wouldn't be meaningful.
	preLate, ok, err := st.GetItem("late-complete")
	if err != nil || !ok {
		t.Fatalf("missing late-complete row before late event: found=%v err=%v", ok, err)
	}
	if preLate.Status != statusErrored {
		t.Fatalf("precondition: expected force-closed row to be errored, got %q — the force-close safety net is not running", preLate.Status)
	}
	if !strings.Contains(preLate.Summary, "turn ended with tool unresolved") {
		t.Fatalf("precondition: expected force-close marker in summary, got %q", preLate.Summary)
	}

	// 4. A stray EventToolComplete arrives LATE for the same
	// tool_use_id. This is the "turn is over, drop the stray" case.
	completeMeta, _ := json.Marshal(map[string]any{"exit_code": 0})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "late-complete",
		Meta: completeMeta, Content: "final stdout", Timestamp: startedAt.Add(2 * time.Second),
	}); err != nil {
		t.Fatalf("late complete: %v", err)
	}

	postLate, ok, err := st.GetItem("late-complete")
	if err != nil || !ok {
		t.Fatalf("missing late-complete row after late event: found=%v err=%v", ok, err)
	}

	// Spec invariant 23: a force-closed `errored` row must stay
	// errored when a stray EventToolComplete lands after turn-complete.
	// The row has settled and the timeline has been rendered; the late
	// event is noise.
	if postLate.Status != statusErrored {
		t.Errorf("spec invariant 23: late EventToolComplete resurrected a force-closed row: status=%q, want %q", postLate.Status, statusErrored)
	}
	// The force-close marker must survive intact — neither stripped
	// nor duplicated by the dropped late completion.
	if !strings.Contains(postLate.Summary, "turn ended with tool unresolved") {
		t.Errorf("spec invariant 23: force-close marker stripped by late completion: %q", postLate.Summary)
	}
	// The pre-late summary and post-late summary must match
	// byte-for-byte: a dropped event writes nothing at all.
	if postLate.Summary != preLate.Summary {
		t.Errorf("spec invariant 23: late completion rewrote summary: pre=%q post=%q", preLate.Summary, postLate.Summary)
	}
	if postLate.UpdatedAt != preLate.UpdatedAt {
		t.Errorf("spec invariant 23: late completion bumped UpdatedAt: pre=%d post=%d", preLate.UpdatedAt, postLate.UpdatedAt)
	}

	// Inline tool_calls must never produce a sibling tool_completion
	// row (invariant 5).
	dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(dones) != 0 {
		t.Errorf("late EventToolComplete spuriously created %d tool_completion siblings (invariant 5 violated)", len(dones))
	}
}

// TestBackgroundTerminalStatus_KilledMapping locks in the Phase 1
// distinction between killed (user-initiated stop) and errored
// (runtime failure). A backgroundTaskTerminalMeta with status=killed
// must map to statusKilled — even when is_error=true (the parser
// stamps is_error on every non-completed terminal, so the status
// precedence has to favour killed over the generic errored bucket).
// Every other non-completed status still collapses to statusErrored
// so an unknown wire value never renders as a success badge.
func TestBackgroundTerminalStatus_KilledMapping(t *testing.T) {
	cases := []struct {
		name string
		in   backgroundTaskTerminalMeta
		want string
	}{
		{
			name: "killed+is_error renders as killed (user stop takes precedence)",
			in:   backgroundTaskTerminalMeta{Status: "killed", IsError: true},
			want: statusKilled,
		},
		{
			name: "bare killed still renders as killed",
			in:   backgroundTaskTerminalMeta{Status: "killed"},
			want: statusKilled,
		},
		{
			name: "failed stays errored",
			in:   backgroundTaskTerminalMeta{Status: "failed", IsError: true},
			want: statusErrored,
		},
		{
			name: "completed stays completed",
			in:   backgroundTaskTerminalMeta{Status: "completed"},
			want: statusCompleted,
		},
		{
			// SDK-normalized form of `killed`: print.ts:2042-2047 maps
			// the XML-form `killed` to `stopped` for SDK consumers.
			// In normal flow, a stash from `task_updated` provides the
			// raw `killed` and this case is unreachable — but if a
			// `task_notification` arrives without a prior task_updated
			// (notification-only path) the SDK form lands directly on
			// the meta. Defensive mapping renders correctly as Stopped
			// rather than collapsing to Failed.
			name: "stopped (SDK-normalized killed) renders as killed",
			in:   backgroundTaskTerminalMeta{Status: "stopped"},
			want: statusKilled,
		},
		{
			// Same as above but with is_error set (the parser stamps
			// is_error=true for every non-completed terminal).
			name: "stopped+is_error still renders as killed",
			in:   backgroundTaskTerminalMeta{Status: "stopped", IsError: true},
			want: statusKilled,
		},
		{
			name: "unknown status falls back to errored",
			in:   backgroundTaskTerminalMeta{Status: "mystery"},
			want: statusErrored,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := backgroundTerminalStatus(tc.in)
			if got != tc.want {
				t.Errorf("backgroundTerminalStatus(%+v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	// statusKilled must be the literal 'killed' — the store CHECK
	// constraint (migration v22) pins this literal, and the frontend
	// renders the Stopped badge off this exact string. A rename would
	// silently slip past the other tests in this file.
	if statusKilled != "killed" {
		t.Errorf("statusKilled = %q, want %q", statusKilled, "killed")
	}
}

// TestAskUserQuestionLifecycle_StartCompletedFlipsToCompleted exercises
// the AskUserQuestion happy path: assistant emits a tool_use for
// AskUserQuestion (parser un-suppressed → EventToolStart), the user
// answers via the in-composer panel, the tool_result echoes back
// (parser un-suppressed → EventToolComplete). The single tool_call row
// flips running → completed in place, and the answer JSON is merged
// into item.meta via the standard tool_result merge path.
func TestAskUserQuestionLifecycle_StartCompletedFlipsToCompleted(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "AskUserQuestion",
		"input": map[string]any{
			"questions": []any{
				map[string]any{
					"id":       "framework",
					"header":   "Framework",
					"question": "Which framework do you want?",
					"options": []any{
						map[string]any{"label": "React", "description": ""},
						map[string]any{"label": "Svelte", "description": ""},
					},
				},
			},
		},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "ask-1",
		ItemType:  "AskUserQuestion",
		Meta:      startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle start: %v", err)
	}

	// Verify the launch row landed at status=running with the
	// questions in item.meta.
	items := findItemsByKind(t, st, "t1", itemKindToolCall)
	if len(items) != 1 {
		t.Fatalf("expected 1 tool_call after launch, got %d", len(items))
	}
	if items[0].Status != statusRunning {
		t.Errorf("launch status = %q, want running", items[0].Status)
	}
	if items[0].ToolName != "AskUserQuestion" {
		t.Errorf("launch toolName = %q, want AskUserQuestion", items[0].ToolName)
	}

	// User answered "Svelte"; CLI echoes back as a tool_result. The
	// completion meta carries the full block including content.
	completeMeta, _ := json.Marshal(map[string]any{
		"is_error": false,
		"tool_result": map[string]any{
			"type":        "tool_result",
			"tool_use_id": "ask-1",
			"content":     `{"answers":{"framework":"Svelte"}}`,
		},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolComplete,
		ThreadID:  "t1",
		ItemID:    "ask-1",
		Meta:      completeMeta,
		Content:   `{"answers":{"framework":"Svelte"}}`,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle complete: %v", err)
	}

	items = findItemsByKind(t, st, "t1", itemKindToolCall)
	if len(items) != 1 {
		t.Fatalf("expected 1 tool_call (in-place mutation), got %d", len(items))
	}
	if items[0].Status != statusCompleted {
		t.Errorf("completion status = %q, want completed", items[0].Status)
	}

	// Both questions (from launch) and answers (from completion)
	// should be present in the merged item.meta — that's how the
	// frontend AskUserQuestionCard renders the per-option check/X
	// grid.
	var meta map[string]any
	if err := json.Unmarshal([]byte(items[0].Meta), &meta); err != nil {
		t.Fatalf("item.meta unmarshal: %v", err)
	}
	if _, ok := meta["input"]; !ok {
		t.Errorf("merged meta missing 'input' (questions): %v", meta)
	}
	if _, ok := meta["tool_result"]; !ok {
		t.Errorf("merged meta missing 'tool_result' (answers): %v", meta)
	}
}

// TestAskUserQuestionLifecycle_InterruptForceClosesToErrored covers
// the stop-mid-question path: when the user clicks Stop while an
// AskUserQuestion is awaiting an answer, the turn ends without a
// matching tool_result. The existing forceCloseOrphanToolCalls safety
// net (invariant 23) flips the orphan tool_call to errored with the
// "turn ended with tool unresolved" summary suffix. Same behavior the
// frontend renders as the failure CompletionBadge for any inline tool.
func TestAskUserQuestionLifecycle_InterruptForceClosesToErrored(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t1",
		TurnIndex: 1,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "AskUserQuestion",
		"input": map[string]any{
			"questions": []any{
				map[string]any{
					"id":       "q",
					"header":   "Q",
					"question": "Pick?",
					"options": []any{
						map[string]any{"label": "a", "description": ""},
						map[string]any{"label": "b", "description": ""},
					},
				},
			},
		},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "ask-interrupted",
		ItemType:  "AskUserQuestion",
		Meta:      startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle start: %v", err)
	}

	// User stops the thread. Turn completes without a matching
	// tool_result for AskUserQuestion.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnComplete,
		ThreadID:  "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}

	item, ok, err := st.GetItem("ask-interrupted")
	if err != nil || !ok {
		t.Fatalf("missing ask-interrupted: found=%v err=%v", ok, err)
	}
	if item.Status != statusErrored {
		t.Errorf("status = %q, want errored (force-close safety net)", item.Status)
	}
	if !strings.Contains(item.Summary, "turn ended with tool unresolved") {
		t.Errorf("summary missing force-close marker: %q", item.Summary)
	}
}

// TestAskUserQuestionLifecycle_PreservesPreviewInQuestions confirms
// the option preview field (added on the wire path for the
// side-by-side mockup-comparison UI) round-trips through the launch
// meta. Without this, persisted rows would lose preview content on
// fork/restore even though the in-composer panel rendered it during
// live interaction.
func TestAskUserQuestionLifecycle_PreservesPreviewInQuestions(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Preview bodies use markers that DON'T match their option labels
	// or descriptions — otherwise a stripped `preview` field would
	// still pass the assertions because the labels/descriptions
	// already contain "Compact" / "Spacious".
	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "AskUserQuestion",
		"input": map[string]any{
			"questions": []any{
				map[string]any{
					"id":       "layout",
					"header":   "Layout",
					"question": "Which layout?",
					"options": []any{
						map[string]any{
							"label":       "Compact",
							"description": "Tight rows.",
							"preview":     "# COMPACT_PREVIEW_BODY\nRow 1\nRow 2",
						},
						map[string]any{
							"label":       "Spacious",
							"description": "Roomy rows.",
							"preview":     "# SPACIOUS_PREVIEW_BODY\n\nRow 1\n\nRow 2",
						},
					},
				},
			},
		},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "ask-preview",
		ItemType:  "AskUserQuestion",
		Meta:      startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle start: %v", err)
	}

	items := findItemsByKind(t, st, "t1", itemKindToolCall)
	if len(items) != 1 {
		t.Fatalf("expected 1 tool_call, got %d", len(items))
	}
	// Distinguishing markers — appear ONLY inside the preview field.
	// If the preview is silently stripped at any layer (parser,
	// triage, persistence), these assertions fail even though the
	// option labels and descriptions still contain the strings
	// "Compact" / "Spacious".
	if !strings.Contains(items[0].Meta, "COMPACT_PREVIEW_BODY") {
		t.Errorf("meta missing first preview body marker: %q", items[0].Meta)
	}
	if !strings.Contains(items[0].Meta, "SPACIOUS_PREVIEW_BODY") {
		t.Errorf("meta missing second preview body marker: %q", items[0].Meta)
	}
}
