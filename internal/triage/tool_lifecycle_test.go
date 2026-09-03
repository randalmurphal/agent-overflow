package triage

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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

func TestToolInputPreviewIgnoresMultiAgentV2Prompt(t *testing.T) {
	input := json.RawMessage(`{"activityKind":"interacted","prompt":"gAAAA-encrypted"}`)
	if got := toolInputPreview(input); got != "" {
		t.Fatalf("toolInputPreview = %q, want empty for MultiAgentV2 activity", got)
	}
	legacy := json.RawMessage(`{"prompt":"Review the parser"}`)
	if got := toolInputPreview(legacy); got != "Review the parser" {
		t.Fatalf("legacy toolInputPreview = %q", got)
	}
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

	upserted := findUpsertedItems(emissions.snapshot())
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
	upserted := findUpsertedItems(emissions.snapshot())
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
// The exit_code-or-is_error fork in CompletionStatus picks errored over
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

// TestToolCompleteWatchTaskKeepsRunningButNotQueueBlocking pins the
// Monitor watch-task flow end to end at the triage/store seam: the
// completion's `watch_task` marker must land on the launch row's meta
// (the keep-running flip does a selective one-key merge), the row still
// counts as live background work (reaper / revert / context-repair
// consumers), but the flush-queue predicate ignores it — a persistent
// watch must not starve a queued user send until session end.
func TestToolCompleteWatchTaskKeepsRunningButNotQueueBlocking(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "Monitor",
		"input":    map[string]any{"command": "bash chain.sh | grep PHASE", "persistent": true},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "watch-tool",
		ItemType: "Monitor", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	completeMeta, _ := json.Marshal(map[string]any{"is_background": true, "watch_task": true})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "watch-tool",
		Meta: completeMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	launches := findItemsByKind(t, st, "t1", itemKindToolCall)
	if len(launches) != 1 {
		t.Fatalf("expected 1 launch row, got %d", len(launches))
	}
	if !launches[0].IsBackground || launches[0].Status != statusRunning {
		t.Fatalf("watch launch must stay running background; got background=%v status=%q", launches[0].IsBackground, launches[0].Status)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(launches[0].Meta), &meta); err != nil {
		t.Fatalf("unmarshal launch meta: %v", err)
	}
	if meta["watch_task"] != true {
		t.Fatalf("watch_task must be merged onto the launch row meta; meta=%v", meta)
	}

	live, err := st.HasLiveBackgroundToolCall("t1")
	if err != nil {
		t.Fatalf("HasLiveBackgroundToolCall: %v", err)
	}
	if !live {
		t.Fatal("a running watch IS live background work (reaper/revert/repair view)")
	}
	blocking, err := st.HasQueueBlockingBackgroundToolCall("t1")
	if err != nil {
		t.Fatalf("HasQueueBlockingBackgroundToolCall: %v", err)
	}
	if blocking {
		t.Fatal("a watch task must not block the flush-queue drain")
	}
}

// TestToolCompleteBackgroundStaysQueueBlocking is the negative guard
// for the watch-task carve-out: an ordinary backgrounded launch (no
// watch_task marker) must block the flush-queue drain exactly as
// before.
func TestToolCompleteBackgroundStaysQueueBlocking(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "Bash",
		"input":    map[string]any{"command": "make check", "run_in_background": true},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-tool",
		ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	completeMeta, _ := json.Marshal(map[string]any{"is_background": true})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "bg-tool",
		Meta: completeMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	blocking, err := st.HasQueueBlockingBackgroundToolCall("t1")
	if err != nil {
		t.Fatalf("HasQueueBlockingBackgroundToolCall: %v", err)
	}
	if !blocking {
		t.Fatal("an ordinary backgrounded launch must still block the flush-queue drain")
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

	preCompletion, _, _ := st.GetThreadItem("t1", "fc-rich")
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

	postCompletion, _, _ := st.GetThreadItem("t1", "fc-rich")
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
// stores that body as a command_output payload so Bash rows share the
// same expandable terminal UI as streamed command output.
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

	item, _, _ := st.GetThreadItem("t1", "bash-1")
	if item.PayloadID == "" {
		t.Fatalf("expected command_output payload attached to inline completion")
	}
	if item.PayloadKind != "command_output" {
		t.Fatalf("payloadKind = %q, want command_output", item.PayloadKind)
	}
	data, err := st.GetPayloadData(item.ThreadID, item.PayloadID)
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if !strings.Contains(string(data), "total 0") {
		t.Errorf("payload data lost stdout body: %q", string(data))
	}
}

func TestInlineBashCompletionStoresFailureMessage(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "Bash",
		"input":    map[string]any{"command": "cat missing.txt"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bash-fail",
		ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	completeMeta, _ := json.Marshal(map[string]any{
		"exit_code": 2,
		"tool_use_result": map[string]any{
			"stderr": "cat: missing.txt: No such file or directory\n",
		},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "bash-fail",
		Meta: completeMeta, Content: "cat: missing.txt: No such file or directory\n", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	item, _, _ := st.GetThreadItem("t1", "bash-fail")
	var meta CommandOutputMeta
	if err := json.Unmarshal([]byte(item.PayloadMeta), &meta); err != nil {
		t.Fatalf("payload meta: %v", err)
	}
	if meta.Command != "cat missing.txt" {
		t.Fatalf("command = %q, want cat missing.txt", meta.Command)
	}
	if meta.ExitCode != 2 {
		t.Fatalf("exitCode = %d, want 2", meta.ExitCode)
	}
	if meta.ErrorMessage != "cat: missing.txt: No such file or directory" {
		t.Fatalf("errorMessage = %q", meta.ErrorMessage)
	}
}

func TestToolCompletionPayloadStoresPreviewMetadata(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "custom_tool",
		"input":    map[string]any{"query": "inspect"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "tool-preview",
		ItemType: "custom_tool", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	content := "first line\nsecond line\n" + strings.Repeat("x", 260)
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "tool-preview",
		ItemType: "custom_tool", Content: content, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	item, found, err := st.GetThreadItem("t1", "tool-preview")
	if err != nil || !found {
		t.Fatalf("item missing: found=%v err=%v", found, err)
	}
	if item.PayloadKind != payloadKindToolCallResult {
		t.Fatalf("payloadKind = %q, want %s", item.PayloadKind, payloadKindToolCallResult)
	}
	var meta struct {
		Preview string `json:"preview"`
	}
	if err := json.Unmarshal([]byte(item.PayloadMeta), &meta); err != nil {
		t.Fatalf("payload meta: %v", err)
	}
	if !strings.HasPrefix(meta.Preview, "first line second line ") {
		t.Fatalf("preview = %q", meta.Preview)
	}
	if utf8.RuneCountInString(meta.Preview) > 240 {
		t.Fatalf("preview length = %d, want capped at 240", utf8.RuneCountInString(meta.Preview))
	}
	if !strings.HasSuffix(meta.Preview, "…") {
		t.Fatalf("preview = %q, want ellipsis", meta.Preview)
	}
}

func TestTruncatePreviewPreservesMultibyteBoundaryAndCapsRunes(t *testing.T) {
	preview := truncatePreview("  "+strings.Repeat("é", 260), 240)

	if !utf8.ValidString(preview) {
		t.Fatalf("preview is not valid utf8: %q", preview)
	}
	if utf8.RuneCountInString(preview) != 240 {
		t.Fatalf("preview rune count = %d, want 240", utf8.RuneCountInString(preview))
	}
	if !strings.HasSuffix(preview, "…") {
		t.Fatalf("preview = %q, want ellipsis", preview)
	}
	if strings.ContainsRune(preview, utf8.RuneError) {
		t.Fatalf("preview contains replacement rune: %q", preview)
	}
}

func TestCommandFromInputPreservesFullCommand(t *testing.T) {
	longCommand := strings.Repeat("echo full-command ", 12)
	input, _ := json.Marshal(map[string]any{"command": longCommand})

	want := strings.TrimSpace(longCommand)
	if got := commandFromInput(input); got != want {
		t.Fatalf("commandFromInput = %q, want full command %q", got, want)
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

	item, _, _ := st.GetThreadItem("t1", "web-1")
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

	item, _, _ := st.GetThreadItem("t1", "wait-1")
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

func TestToolCompletionPreservesCodexWaitStartReceiversSeparatelyOnReplay(t *testing.T) {
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

	item, _, _ := st.GetThreadItem("t1", "wait-1")
	var meta map[string]any
	if err := json.Unmarshal([]byte(item.Meta), &meta); err != nil {
		t.Fatalf("parse item meta: %v", err)
	}
	input, ok := meta["input"].(map[string]any)
	if !ok {
		t.Fatalf("input missing: %#v", meta)
	}
	receivers, ok := input["receiverThreadIds"].([]any)
	if !ok || len(receivers) != 1 || receivers[0] != "child-1" {
		t.Fatalf("receiverThreadIds = %#v, want completion ids", input["receiverThreadIds"])
	}
	requestedReceivers, ok := input["requestedReceiverThreadIds"].([]any)
	if !ok || len(requestedReceivers) != 3 {
		t.Fatalf("requestedReceiverThreadIds = %#v, want three start ids", input["requestedReceiverThreadIds"])
	}
	agentsStates, ok := input["agentsStates"].(map[string]any)
	if !ok || len(agentsStates) != 1 {
		t.Fatalf("agentsStates = %#v, want only completed child state", input["agentsStates"])
	}
}

func TestCodexWaitStartSnapshotsActiveReceiversWhenWireTargetsAreMissing(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	if err := st.UpdateProvider("t1", "codex"); err != nil {
		t.Fatalf("set provider: %v", err)
	}

	seedLaunch := func(id, parentID, meta string, itemIndex int) {
		t.Helper()
		if err := st.InsertItem(store.Item{
			ID: id, ThreadID: "t1", TurnIndex: 0, ItemIndex: itemIndex,
			Kind: itemKindToolCall, Role: "assistant", Status: statusCompleted,
			ParentID: parentID, ToolName: "collab_agent", IsBackground: true,
			Meta: meta, CreatedAt: int64(1000 + itemIndex), UpdatedAt: int64(1000 + itemIndex),
		}); err != nil {
			t.Fatalf("seed launch %s: %v", id, err)
		}
	}
	seedLaunch("spawn-a", "", `{"live_background_active":true,"input":{"tool":"spawn_agent","receiverThreadIds":["child-a"],"newAgentNickname":"Ada","newAgentRole":"reviewer"}}`, 0)
	seedLaunch("spawn-multi", "", `{"live_background_active":true,"codex_child_terminal_statuses":{"child-b":"completed"},"input":{"tool":"spawn_agent","receiverThreadIds":["child-b","child-c"],"receiverAgents":[{"threadId":"child-c","agentNickname":"Curie","agentRole":"default"}]}}`, 1)
	seedLaunch("spawn-duplicate", "", `{"live_background_active":true,"input":{"tool":"spawn_agent","receiverThreadIds":["child-c"]}}`, 2)
	seedLaunch("spawn-nested", "parent-launch", `{"live_background_active":true,"input":{"tool":"spawn_agent","receiverThreadIds":["nested-child"]}}`, 3)

	startMeta := json.RawMessage(`{"toolName":"wait_agent","input":{"tool":"wait_agent"}}`)
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "wait-root",
		ItemType: "wait_agent", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("root wait start: %v", err)
	}

	rootWait, found, err := st.GetThreadItem("t1", "wait-root")
	if err != nil || !found {
		t.Fatalf("root wait missing: found=%v err=%v", found, err)
	}
	if got, want := receiverThreadIDsFromItemMeta(json.RawMessage(rootWait.Meta)), []string{"child-a", "child-c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("root wait snapshot = %+v, want %+v", got, want)
	}
	if got, want := receiverAgentsFromItemMeta(json.RawMessage(rootWait.Meta)), []codexWaitReceiverAgent{
		{ThreadID: "child-a", AgentNickname: "Ada", AgentRole: "reviewer"},
		{ThreadID: "child-c", AgentNickname: "Curie", AgentRole: "default"},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("root wait receiver labels = %+v, want %+v", got, want)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "wait-nested",
		ItemType: "wait_agent", ParentToolUseID: "parent-launch", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("nested wait start: %v", err)
	}
	nestedWait, found, err := st.GetThreadItem("t1", "wait-nested")
	if err != nil || !found {
		t.Fatalf("nested wait missing: found=%v err=%v", found, err)
	}
	if got, want := receiverThreadIDsFromItemMeta(json.RawMessage(nestedWait.Meta)), []string{"nested-child"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("nested wait snapshot = %+v, want %+v", got, want)
	}

	completeMeta := json.RawMessage(`{"toolName":"wait_agent","item_status":"completed","input":{"tool":"wait_agent","receiverThreadIds":["child-a"],"agentsStates":{"child-a":{"status":"completed"}}}}`)
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "wait-root",
		ItemType: "wait_agent", Meta: completeMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("root wait complete: %v", err)
	}
	rootWait, _, _ = st.GetThreadItem("t1", "wait-root")
	var persisted struct {
		Input struct {
			ReceiverThreadIDs          []string                 `json:"receiverThreadIds"`
			RequestedReceiverThreadIDs []string                 `json:"requestedReceiverThreadIds"`
			RequestedReceiverAgents    []codexWaitReceiverAgent `json:"requestedReceiverAgents"`
		} `json:"input"`
	}
	if err := json.Unmarshal([]byte(rootWait.Meta), &persisted); err != nil {
		t.Fatalf("decode completed root wait: %v", err)
	}
	if got, want := persisted.Input.ReceiverThreadIDs, []string{"child-a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("completion receivers = %+v, want %+v", got, want)
	}
	if got, want := persisted.Input.RequestedReceiverThreadIDs, []string{"child-a", "child-c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("requested receiver snapshot = %+v, want %+v", got, want)
	}
	if got, want := persisted.Input.RequestedReceiverAgents, []codexWaitReceiverAgent{
		{ThreadID: "child-a", AgentNickname: "Ada", AgentRole: "reviewer"},
		{ThreadID: "child-c", AgentNickname: "Curie", AgentRole: "default"},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("requested receiver labels = %+v, want %+v", got, want)
	}
}

func TestCodexWaitStartDoesNotOverwriteExplicitReceiverTargets(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	if err := st.UpdateProvider("t1", "codex"); err != nil {
		t.Fatalf("set provider: %v", err)
	}
	if err := st.InsertItem(store.Item{
		ID: "spawn-active", ThreadID: "t1", TurnIndex: 0, ItemIndex: 0,
		Kind: itemKindToolCall, Role: "assistant", Status: statusCompleted,
		ToolName: "collab_agent", IsBackground: true,
		Meta:      `{"live_background_active":true,"input":{"tool":"spawn_agent","receiverThreadIds":["inferred-child"]}}`,
		CreatedAt: 1000, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("seed active launch: %v", err)
	}

	startMeta := json.RawMessage(`{"toolName":"wait_agent","input":{"tool":"wait_agent","requestedReceiverThreadIds":["explicit-child"]}}`)
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "wait-explicit",
		ItemType: "wait_agent", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("wait start: %v", err)
	}
	item, found, err := st.GetThreadItem("t1", "wait-explicit")
	if err != nil || !found {
		t.Fatalf("wait missing: found=%v err=%v", found, err)
	}
	var persisted struct {
		Input struct {
			ReceiverThreadIDs          []string `json:"receiverThreadIds"`
			RequestedReceiverThreadIDs []string `json:"requestedReceiverThreadIds"`
		} `json:"input"`
	}
	if err := json.Unmarshal([]byte(item.Meta), &persisted); err != nil {
		t.Fatalf("decode wait: %v", err)
	}
	if len(persisted.Input.ReceiverThreadIDs) != 0 {
		t.Fatalf("inferred receivers overwrote explicit targets: %+v", persisted.Input.ReceiverThreadIDs)
	}
	if got, want := persisted.Input.RequestedReceiverThreadIDs, []string{"explicit-child"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("requested receivers = %+v, want %+v", got, want)
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

	item, _, _ := st.GetThreadItem("t1", "mcp-1")
	if item.PayloadID == "" {
		t.Fatal("expected MCP result payload")
	}
	data, err := st.GetPayloadData(item.ThreadID, item.PayloadID)
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
// THAT row via UpsertItem (no second item appended). Result: one
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

	item, _, err := st.GetThreadItem("t1", "tool-bg-meta")
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

	item, _, err := st.GetThreadItem("t1", "agent-tool-1")
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
	item2, _, _ := st.GetThreadItem("t1", "agent-tool-1")
	if item2.UpdatedAt != item.UpdatedAt {
		t.Errorf("redundant meta update bumped UpdatedAt: was=%d now=%d", item.UpdatedAt, item2.UpdatedAt)
	}
}

func TestCodexSpawnLabelMetaUpdatePreservesFullReceiverList(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	if err := st.UpdateProvider("t1", "codex"); err != nil {
		t.Fatalf("set provider: %v", err)
	}

	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "collab_agent",
		"input": map[string]any{
			"tool":              "spawn_agent",
			"prompt":            "Run sleep jobs",
			"model":             "gpt-5.6-sol",
			"reasoningEffort":   "high",
			"receiverThreadIds": []string{"child-1", "child-2"},
		},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "spawn-1",
		ItemType: "collab_agent", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "spawn-1",
		ItemType: "collab_agent", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool complete: %v", err)
	}

	updateMeta, _ := json.Marshal(map[string]any{
		"meta_update_only": true,
		"toolName":         "collab_agent",
		"input": map[string]any{
			"tool":              "spawn_agent",
			"model":             "gpt-5.6-luna",
			"reasoningEffort":   "low",
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

	item, _, err := st.GetThreadItem("t1", "spawn-1")
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	var persistedMeta struct {
		Input struct {
			ReceiverThreadIDs []string `json:"receiverThreadIds"`
			NewAgentNickname  string   `json:"newAgentNickname"`
			NewAgentRole      string   `json:"newAgentRole"`
			Model             string   `json:"model"`
			ReasoningEffort   string   `json:"reasoningEffort"`
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
	if persistedMeta.Input.Model != "gpt-5.6-luna" || persistedMeta.Input.ReasoningEffort != "low" {
		t.Fatalf("effective profile = %q/%q, want gpt-5.6-luna/low", persistedMeta.Input.Model, persistedMeta.Input.ReasoningEffort)
	}
}

func TestMirroredCommandRowChangesToSkillInPlace(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "Command",
		"input":    map[string]any{"command": "/code-review", "args": "high"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "claude-command:cmd-1",
		ItemType: "Command", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("provisional command start: %v", err)
	}

	skillMeta, _ := json.Marshal(map[string]any{
		"meta_update_only": true,
		"toolName":         "Skill",
		"input":            map[string]any{"skill": "code-review", "args": "high"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "claude-command:cmd-1",
		ItemType: "Skill", Meta: skillMeta, Timestamp: time.Now().Add(time.Millisecond),
	}); err != nil {
		t.Fatalf("skill classification update: %v", err)
	}

	item, found, err := st.GetThreadItem("t1", "claude-command:cmd-1")
	if err != nil || !found {
		t.Fatalf("get classified command row: found=%v err=%v", found, err)
	}
	if item.ToolName != "Skill" || item.Status != statusRunning {
		t.Fatalf("classified command row = tool %q status %q, want running Skill", item.ToolName, item.Status)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(item.Meta), &meta); err != nil {
		t.Fatalf("decode classified command meta: %v", err)
	}
	input, _ := meta["input"].(map[string]any)
	if input["skill"] != "code-review" {
		t.Fatalf("classified command input = %+v", input)
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
	// The killed terminal is STASHED, not dropped: the launch row may
	// still land later (subagent transcript projection lags the main
	// wire), and the stash is what settles it then. The session-end
	// settle prunes stashes whose row never materializes.
	if _, found, err := st.GetPendingBackgroundTerminal("t1", "missing-killed-task"); err != nil {
		t.Fatalf("read stash: %v", err)
	} else if !found {
		t.Fatal("killed task_updated without a launch row must be stashed for the late-arriving row")
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
	if done.ID != ToolCompletionID("bg-insert") {
		t.Errorf("sibling id = %q, want %q", done.ID, ToolCompletionID("bg-insert"))
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
	for _, item := range filterItemEventUpserts(emissions.snapshot()) {
		if item.ID == ToolCompletionID("bg-insert") {
			siblingUpserts++
		}
	}
	if siblingUpserts < 2 {
		t.Errorf("expected at least 2 sibling upsert emissions (insert + idempotent refresh), got %d", siblingUpserts)
	}
}

// TestHandleEventBackgroundTaskTerminal_TaskUpdatedStashesNoSibling
// pins the post-stash refactor invariant: source=task_updated is the
// HOST process exit signal, NOT the agent observation. It must stash
// a pending_background_task_terminals row but MUST NOT write the
// chat-side `tool_completion` sibling — the agent has not yet seen
// the completion. The stash carries exit_code / end_time / output_file
// forward so the observation-event drain (TaskOutput or
// task_notification) can merge the host outcome into the persisted
// sibling. The launch row stays "running" in the tray until that
// observation arrives; in practice task_updated and the observation
// arrive in the same wire flush batch so the gap is sub-perceptual.
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

	// Launch stays visible — the stash never hides it.
	live, err := st.ListLiveBackgroundTasks("t1", 0)
	if err != nil {
		t.Fatalf("list live: %v", err)
	}
	sawLaunch := false
	for _, row := range live {
		if row.ID == "bg-stash" {
			sawLaunch = true
		}
	}
	if !sawLaunch {
		t.Fatalf("launch missing from tray after task_updated stash; want it visible until the observation event lands: %+v", live)
	}

	// Frontend nudge: provider:background_task_state with state=exited.
	sawExited := false
	for _, e := range emissions.snapshot() {
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
	emissions.reset()
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
	for _, e := range emissions.snapshot() {
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

// A background task launched INSIDE a subagent gets its completion
// sibling on the launch's turn, not the thread's current one. Every
// other row under that launch is pinned to the launch's turn (invariant
// 10), so a sibling parked on the main thread's later turn sorts after
// every row the agent writes afterwards — the "done" row rode the tail
// of the agent's newest activity run for the rest of its life (live
// 2026-09-01). The top-level rule above is unchanged.
func TestHandleEventBackgroundTaskTerminal_ScopedSiblingStaysOnLaunchTurn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	startAgentLaunch(t, router, "t1", "agent-1", "", "task-agent")

	startMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Bash",
		"is_background": true,
		"input":         map[string]any{"command": "make apk", "run_in_background": true},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-child",
		ItemType: "Bash", Meta: startMeta, ParentToolUseID: "agent-1", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("seed scoped launch: %v", err)
	}
	// The agent keeps writing rows on the launch's turn after the shell
	// backgrounds; the sibling has to sort AFTER this one, not after
	// everything the agent will ever write.
	deliverSubagentBlock(t, router, "t1", "agent-1", "msg_after#0", "text", "still working")

	// The main thread has moved on by the time the shell reports.
	seedOpenTurn(t, router, st, "t1", 2)
	terminalMeta, _ := json.Marshal(map[string]any{
		"task_id":     "tsk-child",
		"tool_use_id": "bg-child",
		"status":      "completed",
		"source":      "task_output",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "bg-child",
		Meta: terminalMeta, Content: "done", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("bg terminal: %v", err)
	}

	completion, ok, err := st.GetThreadItem("t1", ToolCompletionID("bg-child"))
	if err != nil || !ok {
		t.Fatalf("lookup completion sibling: ok=%v err=%v", ok, err)
	}
	if completion.TurnIndex != 0 {
		t.Fatalf("scoped completion turn_index = %d, want 0 (the launch's turn)", completion.TurnIndex)
	}
	if completion.ParentID != "agent-1" {
		t.Fatalf("scoped completion parent_id = %q, want agent-1", completion.ParentID)
	}
	children := childrenOfLaunch(t, st, "t1", "agent-1", 0)
	ids := childIDs(children)
	if len(ids) == 0 || ids[len(ids)-1] != completion.ID {
		t.Fatalf("completion sibling must append at the scope's tail, children = %v", ids)
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
	meta, err := st.GetPayloadMeta(done.ThreadID, done.PayloadID)
	if err != nil {
		t.Fatalf("read enriched payload meta: %v", err)
	}
	if strings.Contains(meta.Meta, "/tmp/bg-output.txt") || strings.Contains(meta.Meta, "outputFile") {
		t.Errorf("command output payload meta leaked output file path: %q", meta.Meta)
	}
	data, err := st.GetPayloadData(done.ThreadID, done.PayloadID)
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
	initial, ok, err := st.GetThreadItem("t1", ToolCompletionID("bg-stable-turn"))
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

	after, ok, err := st.GetThreadItem("t1", ToolCompletionID("bg-stable-turn"))
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
// contract: a recoverable Claude background launch (running, no
// completion sibling) gets a synthesized session_died `tool_completion`
// sibling directly so the tray clears and the chat row shows `killed`.
// Re-running the recovery is idempotent: the second pass sees the
// now-existing sibling and skips the launch.
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

// Boot-time recovery is the third transition the workspace-change lock
// has to hear about: the launches it retires were `running` in the store
// a moment ago, so a client that mounted before the sweep finished holds
// a lock answer the sweep just invalidated. Each recovered launch nudges
// through writeBackgroundCompletionSibling, which is why the sweep needs
// no post-pass emit of its own — and could not have one, since the
// channel is thread-keyed and the sweep spans threads.
func TestRecoverOrphanedBackgroundTasksNudgesTheWorkspaceLock(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

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
	emissions.reset()

	recovered, err := router.RecoverOrphanedBackgroundTasks()
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("recovered = %d, want 1", recovered)
	}
	if n := countEvents(emissions.snapshot(), "provider:background_tasks_changed"); n != 1 {
		t.Fatalf("recovery emitted %d background_tasks_changed, want 1 (%+v)", n, emissions.snapshot())
	}
}

// TestRecoverOrphanedBackgroundTasksWithoutTaskID pins that a launch
// carrying no task_id is still recovered. claude-tui backgrounded tools
// never receive a task_started meta merge, so their launches are
// is_background=1 with NO task_id; the synthetic completion sibling is
// keyed off the LAUNCH id (backgroundCompletionID), so no task_id is
// needed. Before the claude-tui fix the store query excluded these and
// they rendered "running" forever after a restart — this is the
// regression guard for that.
func TestRecoverOrphanedBackgroundTasksWithoutTaskID(t *testing.T) {
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
	if recovered != 1 {
		t.Fatalf("recovered = %d, want 1 (no-task-id launch must still recover)", recovered)
	}

	// Sibling materialised, keyed off the launch id (not a task_id), with
	// status=killed since there was no host-reported exit.
	dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(dones) != 1 {
		t.Fatalf("expected 1 recovery sibling, got %+v", dones)
	}
	if want := backgroundCompletionID("bg-no-task-id", ""); dones[0].ID != want {
		t.Errorf("sibling ID = %q, want %q (keyed off launch id)", dones[0].ID, want)
	}
	if dones[0].CompletionOf != "bg-no-task-id" {
		t.Errorf("CompletionOf = %q, want bg-no-task-id", dones[0].CompletionOf)
	}
	if dones[0].Status != statusKilled {
		t.Errorf("Status = %q, want %q", dones[0].Status, statusKilled)
	}

	// Idempotent: the second pass sees the sibling and recovers nothing.
	recovered2, err := router.RecoverOrphanedBackgroundTasks()
	if err != nil {
		t.Fatalf("recover (2nd): %v", err)
	}
	if recovered2 != 0 {
		t.Errorf("idempotency broken: 2nd pass recovered %d, want 0", recovered2)
	}
	if dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone); len(dones) != 1 {
		t.Errorf("idempotency broken: sibling count after 2nd pass = %d, want 1", len(dones))
	}
}

// TestRecoverOrphanedBackgroundTasksDrainsStash pins the regression
// guard for the "old session left a stash row stranded" path. Before
// the fix, `ListLiveBackgroundTasks` hid stash-affected launches via
// a NOT EXISTS predicate; that predicate was dropped so the tray
// could pair the launch with the synthetic completion item. But the
// startup recovery sweep also skipped stash-affected launches, which
// meant a stash row stranded from a prior app session would leave
// its launch visible as "running" forever on next boot.
//
// Now recovery drains the stash and uses its data — status, exit
// code, output file — to write the session_died sibling. The user
// sees the real outcome the host captured rather than a generic
// stopped state.
func TestRecoverOrphanedBackgroundTasksDrainsStash(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Same as TestRecoverOrphanedBackgroundTasks: seed the launch with
	// a task_id meta merge.
	startMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Bash",
		"is_background": true,
		"input":         map[string]any{"command": "long-running"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-stash-orphan",
		ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("seed launch: %v", err)
	}
	taskStartedMeta, _ := json.Marshal(map[string]any{"task_id": "tsk-stash-orphan"})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-stash-orphan",
		Meta: taskStartedMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("task_started meta merge: %v", err)
	}

	// Stage a stash entry representing the previous app session's
	// task_updated{completed} that never got drained because the
	// observation event arrived after the session died.
	exit := int64(0)
	if err := st.UpsertPendingBackgroundTerminal(store.PendingBackgroundTaskTerminal{
		ThreadID:   "t1",
		TaskID:     "tsk-stash-orphan",
		ToolUseID:  "bg-stash-orphan",
		Status:     "completed",
		ExitCode:   &exit,
		OutputFile: "/tmp/stash-output.txt",
		Source:     "task_updated",
		CreatedAt:  100, // ancient — would be filtered by synth retention
	}); err != nil {
		t.Fatalf("seed stash: %v", err)
	}

	recovered, err := router.RecoverOrphanedBackgroundTasks()
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("recovered = %d, want 1", recovered)
	}

	// Sibling materialised with the stash's status — "completed" with
	// exit 0 maps to statusCompleted, not statusKilled.
	dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(dones) != 1 {
		t.Fatalf("expected 1 recovery sibling, got %+v", dones)
	}
	if dones[0].CompletionOf != "bg-stash-orphan" {
		t.Errorf("CompletionOf = %q, want bg-stash-orphan", dones[0].CompletionOf)
	}
	if dones[0].Status != statusCompleted {
		t.Errorf("Status = %q, want %q (merged from stash, not the no-stash 'killed' default)", dones[0].Status, statusCompleted)
	}

	// Stash row is gone — the drain is atomic, no half-state left.
	if _, found, err := st.GetPendingBackgroundTerminal("t1", "tsk-stash-orphan"); err != nil {
		t.Fatalf("read stash: %v", err)
	} else if found {
		t.Error("recovery should have drained the stash")
	}

	// Tray no longer shows the launch as "running alone" — the new
	// sibling pairs with it.
	live, err := st.ListLiveBackgroundTasks("t1", 0)
	if err != nil {
		t.Fatalf("list live: %v", err)
	}
	var sawLaunch, sawSibling bool
	for _, row := range live {
		switch row.ID {
		case "bg-stash-orphan":
			sawLaunch = true
		case backgroundCompletionID("bg-stash-orphan", "tsk-stash-orphan"):
			sawSibling = true
		}
	}
	if !sawLaunch || !sawSibling {
		t.Errorf("expected launch + recovery sibling in tray, sawLaunch=%v sawSibling=%v", sawLaunch, sawSibling)
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
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    startedAt.Add(500 * time.Millisecond),
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}

	// Sanity: the force-close safety net ran and left the row errored
	// with the force-close marker — without this, the later
	// resurrection assertion wouldn't be meaningful.
	preLate, ok, err := st.GetThreadItem("t1", "late-complete")
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

	postLate, ok, err := st.GetThreadItem("t1", "late-complete")
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
// so an unknown wire value never renders as a successful row.
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
	// renders the stopped RowError copy off this exact string. A rename
	// would silently slip past the other tests in this file.
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
// "turn ended with tool unresolved" summary suffix. The frontend
// renders the same state as an error indicator plus RowError for any
// inline tool.
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
		Kind:         provider.EventTurnComplete,
		ThreadID:     "t1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}

	item, ok, err := st.GetThreadItem("t1", "ask-interrupted")
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

// TestBackgroundCompletionOrdersBeforeLaterMainTextDespiteSubagentStream is the
// scope-aware refinement of invariant 11. A backgrounded Agent completes while a
// DIFFERENT scope (a concurrent backgrounded subagent) is mid-stream, then the
// main loop emits a NEW main-scope text row. The completion ARRIVED before that
// text, so it must sort before it.
//
// The interrupt queue must defer a main-scope completion only behind a
// SAME-scope (main) stream, never behind a subagent-scope stream — a main-scope
// "-> done" row can't fragment subagent-nested text. The old thread-wide check
// queued the completion behind the subagent stream; because backgrounded
// subagents stream continuously, the queue drained only at idle, landing the
// completion AFTER the later main text.
//
// Reproduces thread 4d82b192 turn 18: "Report CPU model -> done" (Agent A)
// rendered after "First back" because A's completion was queued behind Agent B's
// still-streaming subagent text and drained past "First back".
func TestBackgroundCompletionOrdersBeforeLaterMainTextDespiteSubagentStream(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	step := func(label string, evt provider.ProviderEvent) {
		t.Helper()
		if err := router.Handle(evt); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
	}

	step("turn start", provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", Timestamp: time.Now(),
	})

	launch := func(itemID, desc string) provider.ProviderEvent {
		meta, _ := json.Marshal(map[string]any{
			"toolName":      "Agent",
			"is_background": true,
			"input":         map[string]any{"description": desc, "run_in_background": true},
		})
		return provider.ProviderEvent{
			Kind: provider.EventToolStart, ThreadID: "t1", ItemID: itemID, ItemType: "Agent",
			Meta: meta, Timestamp: time.Now(),
		}
	}

	// Two backgrounded Agent launches (main scope).
	step("launch A", launch("agentA", "Report CPU model"))
	step("launch B", launch("agentB", "Report OS name"))

	// Agent B's subagent is mid-stream: an open text block in B's (non-main)
	// scope. This bumps the thread-wide streaming counter.
	step("subagent B text", provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1", Role: "assistant",
		Content: "checking os-release", ParentToolUseID: "agentB", Timestamp: time.Now(),
	})

	// Agent A reports done (main-scope completion) WHILE B streams. It must not
	// defer behind B's subagent stream.
	completionMeta, _ := json.Marshal(map[string]any{
		"task_id": "taskA", "tool_use_id": "agentA", "status": "completed", "source": "task_output",
	})
	step("A completion", provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "agentA",
		Meta: completionMeta, Content: `Agent "Report CPU model" completed`, Timestamp: time.Now(),
	})

	// Main loop then emits its acknowledgment — a NEW main-scope text row that
	// arrived AFTER A's completion and must therefore sort after it.
	step("First back", provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1", Role: "assistant",
		Content: "First back — Agent A done.", Timestamp: time.Now(),
	})

	// Settle both streams so any queued item drains.
	step("subagent stop", provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: "t1", ParentToolUseID: "agentB",
		Meta: json.RawMessage(`{"index":0,"blockType":"text"}`), Content: "checking os-release",
		ContentPresent: true, Timestamp: time.Now(),
	})
	step("main stop", provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: "t1",
		Meta: json.RawMessage(`{"index":0,"blockType":"text"}`), Content: "First back — Agent A done.",
		ContentPresent: true, Timestamp: time.Now(),
	})
	router.WaitForPendingSettles()

	dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(dones) != 1 {
		t.Fatalf("expected 1 background_done sibling, got %d", len(dones))
	}
	completion := dones[0]

	var firstBack store.Item
	var found bool
	for _, it := range findItemsByKind(t, st, "t1", itemKindAssistantText) {
		if it.ParentID == "" && strings.HasPrefix(it.Summary, "First back") {
			firstBack, found = it, true
		}
	}
	if !found {
		t.Fatal("'First back' main text row not found")
	}

	beforeFirstBack := completion.TurnIndex < firstBack.TurnIndex ||
		(completion.TurnIndex == firstBack.TurnIndex && completion.ItemIndex < firstBack.ItemIndex)
	if !beforeFirstBack {
		t.Fatalf("A completion (turn %d idx %d) must sort BEFORE 'First back' (turn %d idx %d): "+
			"it arrived first but was deferred behind the subagent stream",
			completion.TurnIndex, completion.ItemIndex, firstBack.TurnIndex, firstBack.ItemIndex)
	}
}

// TestSameScopeBackgroundCompletionStillDefersBehindOwnSubagentStream is the
// positive control for the scope-aware deferral: the fix must NOT over-correct.
// A completion in the SAME scope as the open stream still has to defer
// (invariant 11) — a row nested under a subagent can't render above that
// subagent's own still-streaming text.
//
// A backgrounded tool runs INSIDE subagent agentB (scope "agentB"); its
// completion arrives while agentB's text is mid-stream (also scope "agentB").
// It must queue, then drain AFTER agentB's text settles. If a future change
// keyed the queue decision on the wrong field (so the completion's scope no
// longer matched the stream's), the mid-stream assertion below would catch it:
// the completion would persist immediately instead of queuing.
func TestSameScopeBackgroundCompletionStillDefersBehindOwnSubagentStream(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	step := func(label string, evt provider.ProviderEvent) {
		t.Helper()
		if err := router.Handle(evt); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
	}

	step("turn start", provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", Timestamp: time.Now(),
	})

	// Top-level backgrounded Agent (scope "").
	agentMeta, _ := json.Marshal(map[string]any{
		"toolName": "Agent", "is_background": true,
		"input": map[string]any{"description": "Inspect host", "run_in_background": true},
	})
	step("launch agentB", provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "agentB", ItemType: "Agent",
		Meta: agentMeta, Timestamp: time.Now(),
	})

	// A backgrounded tool launched INSIDE agentB: its ParentID resolves to
	// "agentB" (the same scope agentB streams under).
	innerMeta, _ := json.Marshal(map[string]any{
		"toolName": "Bash", "is_background": true,
		"input": map[string]any{"command": "sleep 5"},
	})
	step("launch inner", provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bgInner", ItemType: "Bash",
		Meta: innerMeta, ParentToolUseID: "agentB", Timestamp: time.Now(),
	})

	// agentB streams text (scope "agentB") — opens the same-scope stream.
	step("agentB text", provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1", Role: "assistant",
		Content: "inner work", ParentToolUseID: "agentB", Timestamp: time.Now(),
	})

	// The inner tool completes WHILE agentB streams. Same scope → must defer.
	innerDoneMeta, _ := json.Marshal(map[string]any{
		"task_id": "taskInner", "tool_use_id": "bgInner", "status": "completed", "source": "task_output",
	})
	step("inner completion", provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "bgInner",
		Meta: innerDoneMeta, ParentToolUseID: "agentB", Content: `Background command "sleep 5" completed`,
		Timestamp: time.Now(),
	})

	// Mid-stream: the completion must be QUEUED, not persisted. (Persisting it
	// here would split agentB's text around it.)
	if queued := findItemsByKind(t, st, "t1", itemKindBackgroundDone); len(queued) != 0 {
		t.Fatalf("same-scope completion persisted mid-stream (got %d background_done rows); "+
			"it must defer behind agentB's own stream", len(queued))
	}

	// agentB keeps streaming, then its block closes — draining the queue.
	step("agentB text 2", provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1", Role: "assistant",
		Content: " continues", ParentToolUseID: "agentB", Timestamp: time.Now(),
	})
	step("agentB stop", provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: "t1", ParentToolUseID: "agentB",
		Meta: json.RawMessage(`{"index":0,"blockType":"text"}`), Content: "inner work continues",
		ContentPresent: true, Timestamp: time.Now(),
	})
	router.WaitForPendingSettles()

	dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(dones) != 1 {
		t.Fatalf("expected 1 background_done after agentB settled, got %d", len(dones))
	}
	completion := dones[0]
	if completion.ParentID != "agentB" {
		t.Fatalf("inner completion ParentID = %q, want \"agentB\" (subagent scope)", completion.ParentID)
	}

	var agentText store.Item
	var found bool
	for _, it := range findItemsByKind(t, st, "t1", itemKindAssistantText) {
		if it.ParentID == "agentB" {
			agentText, found = it, true
		}
	}
	if !found {
		t.Fatal("agentB subagent text row not found")
	}
	if !(completion.TurnIndex == agentText.TurnIndex && completion.ItemIndex > agentText.ItemIndex) {
		t.Fatalf("same-scope completion (turn %d idx %d) must sort AFTER agentB's text (turn %d idx %d): "+
			"it arrived mid-stream and must not split the subagent's message",
			completion.TurnIndex, completion.ItemIndex, agentText.TurnIndex, agentText.ItemIndex)
	}
}

// TestToolCompleteWatchTaskMergesOntoAlreadyBackgroundLaunch — the
// keep-running flip's first arm (promote foreground→background) merged
// `watch_task`, but a launch that was ALREADY background at start took
// the second arm, whose early return silently lost the marker. §E7's
// real Monitor launch carries no run_in_background today (the ack is
// what backgrounds it), so this is a tripwire for the future CLI that
// marks the launch up front: watch-ness feeds the flush-queue predicate
// (HasQueueBlockingBackgroundToolCall), and losing it would let a
// queued user send starve behind a persistent watch until session end.
func TestToolCompleteWatchTaskMergesOntoAlreadyBackgroundLaunch(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// run_in_background:true marks the launch background AT START —
	// the shape the current CLI does not send for Monitor, and the one
	// that used to skip the merge.
	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "Monitor",
		"input": map[string]any{
			"command": "bash chain.sh | grep PHASE", "persistent": true,
			"run_in_background": true,
		},
		"is_background": true,
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "watch-bg",
		ItemType: "Monitor", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	completeMeta, _ := json.Marshal(map[string]any{"is_background": true, "watch_task": true})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "watch-bg",
		Meta: completeMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	launches := findItemsByKind(t, st, "t1", itemKindToolCall)
	if len(launches) != 1 {
		t.Fatalf("expected 1 launch row, got %d", len(launches))
	}
	if !launches[0].IsBackground || launches[0].Status != statusRunning {
		t.Fatalf("watch launch must stay running background; got background=%v status=%q",
			launches[0].IsBackground, launches[0].Status)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(launches[0].Meta), &meta); err != nil {
		t.Fatalf("unmarshal launch meta: %v", err)
	}
	if meta["watch_task"] != true {
		t.Fatalf("watch_task must be merged even when the launch was already background; meta=%v", meta)
	}
}

// TestToolCompleteUnflaggedSettlesFlaggedClaudeLaunch pins the completion
// path's authority on a Claude thread: the launch row's is_background
// came from the tool_use INPUT (`run_in_background:true`), and a
// completion the parser did NOT classify as a backgrounding ack — a hook
// deny, a permission denial — settles the row in place and clears the
// flag. Before this the row stayed `running` forever with no task_id,
// and a top-level one blocked the idle reaper and the flush queue for
// the life of the session (2026-09-02).
func TestToolCompleteUnflaggedSettlesFlaggedClaudeLaunch(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	startMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Bash",
		"input":         map[string]any{"command": "make apk", "run_in_background": true},
		"is_background": true,
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "refused-tool",
		ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if live, err := st.HasLiveBackgroundToolCall("t1"); err != nil || !live {
		t.Fatalf("precondition: flagged launch counts as live background work (live=%v err=%v)", live, err)
	}

	completeMeta, _ := json.Marshal(map[string]any{"is_error": true})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "refused-tool",
		Content: "Permission to use Bash has been denied", Meta: completeMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	launches := findItemsByKind(t, st, "t1", itemKindToolCall)
	if len(launches) != 1 {
		t.Fatalf("expected 1 launch row, got %d", len(launches))
	}
	if launches[0].Status != statusErrored {
		t.Errorf("status = %q, want %q (settled in place with the result it got)", launches[0].Status, statusErrored)
	}
	if launches[0].IsBackground {
		t.Error("is_background column must be cleared: no task ever started")
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(launches[0].Meta), &meta); err != nil {
		t.Fatalf("unmarshal launch meta: %v", err)
	}
	if meta["is_background"] != false {
		t.Errorf("stored meta is_background = %v, want false", meta["is_background"])
	}
	if live, err := st.HasLiveBackgroundToolCall("t1"); err != nil || live {
		t.Fatalf("a refused launch must not count as live background work (live=%v err=%v)", live, err)
	}
	running, err := st.ListRunningBackgroundToolCalls("t1")
	if err != nil {
		t.Fatalf("ListRunningBackgroundToolCalls: %v", err)
	}
	if len(running) != 0 {
		t.Fatalf("reaper view must be empty, got %d rows", len(running))
	}
}

// TestToolCompleteBackgroundAckStampsTaskID pins the ack's task id
// landing on the launch row when `system/task_started` never did
// (reconnect gap, or a sidechain row the correlation hold could not
// reach): the tray row then has something to stop by and the later
// terminal can resolve to it. A task_started that DID land wins — first
// non-empty value stays.
func TestToolCompleteBackgroundAckStampsTaskID(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	startMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Bash",
		"input":         map[string]any{"command": "make apk", "run_in_background": true},
		"is_background": true,
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "acked-tool",
		ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	completeMeta, _ := json.Marshal(map[string]any{"is_background": true, "task_id": "bkulztq41"})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "acked-tool",
		Meta: completeMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	launches := findItemsByKind(t, st, "t1", itemKindToolCall)
	if len(launches) != 1 {
		t.Fatalf("expected 1 launch row, got %d", len(launches))
	}
	if !launches[0].IsBackground || launches[0].Status != statusRunning {
		t.Fatalf("acked launch must stay running background; got background=%v status=%q", launches[0].IsBackground, launches[0].Status)
	}
	if got := TaskIDFromItemMeta(launches[0].Meta); got != "bkulztq41" {
		t.Fatalf("meta.task_id = %q, want bkulztq41", got)
	}

	// A second ack-shaped completion naming a different id must not
	// overwrite the bound one.
	completeMeta, _ = json.Marshal(map[string]any{"is_background": true, "task_id": "other"})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "acked-tool",
		Meta: completeMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("second complete: %v", err)
	}
	launches = findItemsByKind(t, st, "t1", itemKindToolCall)
	if got := TaskIDFromItemMeta(launches[0].Meta); got != "bkulztq41" {
		t.Fatalf("meta.task_id = %q after second ack, want the first binding kept", got)
	}
}

// TestToolCompleteUnflaggedKeepsCodexLaunchFlag is the Codex-side guard
// for the Claude rule above: a Codex row's is_background is stamped by
// the projector from wire-typed signals (invariant 25), never by its
// completion, so an unflagged completion must leave a flagged Codex
// launch exactly as it was.
func TestToolCompleteUnflaggedKeepsCodexLaunchFlag(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexTestThread(t, st, "c1")

	startMeta, _ := json.Marshal(map[string]any{
		"toolName":      "commandExecution",
		"input":         map[string]any{"command": "sleep 100"},
		"is_background": true,
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "c1", ItemID: "codex-bg",
		ItemType: "commandExecution", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "c1", ItemID: "codex-bg",
		Meta: json.RawMessage(`{}`), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	launches := findItemsByKind(t, st, "c1", itemKindToolCall)
	if len(launches) != 1 {
		t.Fatalf("expected 1 launch row, got %d", len(launches))
	}
	if !launches[0].IsBackground || launches[0].Status != statusRunning {
		t.Fatalf("Codex launch flag is projector-owned; got background=%v status=%q", launches[0].IsBackground, launches[0].Status)
	}
}
