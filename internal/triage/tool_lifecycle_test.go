package triage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// findUpsertedItems pulls every store.Item that triage published on the
// provider:item_upsert channel, in emission order. Tests assert against
// this slice to verify the lifecycle item flows into the frontend's
// reconciliation pipeline as it would in production.
func findUpsertedItems(emissions []emitted) []store.Item {
	var out []store.Item
	for _, e := range emissions {
		if e.eventName != "provider:item_upsert" {
			continue
		}
		if item, ok := e.data.(store.Item); ok {
			out = append(out, item)
		}
	}
	return out
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

	upserted := findUpsertedItems(*emissions)
	if len(upserted) != 1 || upserted[0].ID != scopedID {
		t.Errorf("expected 1 upsert for %s, got %+v", scopedID, upserted)
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

// TestToolCompleteOnBackgroundedAppendsCompletion is the keystone test
// for the two-row pattern. The launch row stays in place
// (is_background=1, status=running) while a NEW background_done row is
// appended carrying the result payload. Frontends render the launch
// inline at its original position and the completion at its real
// completion time — the launch is never mutated.
func TestToolCompleteOnBackgroundedAppendsCompletion(t *testing.T) {
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

	dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(dones) != 1 {
		t.Fatalf("expected 1 background_done item, got %d", len(dones))
	}
	done := dones[0]
	if done.CompletionOf != "bg-tool" {
		t.Errorf("CompletionOf = %q, want %q", done.CompletionOf, "bg-tool")
	}
	if !done.IsBackground {
		t.Error("background_done IsBackground = false")
	}
	if done.Status != statusCompleted {
		t.Errorf("background_done Status = %q, want completed", done.Status)
	}
	if done.PayloadID == "" {
		t.Error("expected background_done to carry a result payload, got none")
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

// TestToolStartIdempotentOnReplay covers Codex's item/started +
// item/updated dual emission. The second start with the same id must
// not collide on the UNIQUE id constraint; instead the row's summary
// refreshes and is_background can flip if the classifier promoted the
// tool to background mid-stream. Status stays as-is so a stray late
// EventToolStart doesn't roll a completed row back to running.
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

	// Item/updated arrives later with refined input + classifier promotion.
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
		"input":         map[string]any{"command": "npm run dev"},
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
	if !strings.Contains(item.Summary, "Bash") || !strings.Contains(item.Summary, "npm run dev") {
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
// scenario) verifies that when a fresh adapter session emits
// EventToolComplete with empty ItemID and only meta.task_id, triage
// looks up the matching tool_call row via items.meta.task_id and
// terminates IT — not some ghost row keyed by empty string.
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
	// fires with only task_id inline, producing an EventToolComplete
	// with empty ItemID + meta.task_id.
	completeMeta, _ := json.Marshal(map[string]any{
		"is_background": true,
		"task_id":       "recov-task",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "",
		Meta: completeMeta, Content: "terminated", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("task_updated completion: %v", err)
	}

	// Launch row stays frozen at running (per background lifecycle),
	// and a tool_completion partner is appended.
	launches := findItemsByKind(t, st, "t1", itemKindToolCall)
	if len(launches) != 1 {
		t.Fatalf("expected 1 tool_call, got %+v", launches)
	}
	if launches[0].ID != "bg-recov" {
		t.Errorf("unexpected launch id %q", launches[0].ID)
	}
	dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(dones) != 1 {
		t.Fatalf("expected 1 tool_completion, got %+v", dones)
	}
	if dones[0].CompletionOf != "bg-recov" {
		t.Errorf("completion_of = %q, want %q", dones[0].CompletionOf, "bg-recov")
	}
}

// TestTaskUpdatedUnmatchedMetaTaskIDIsDropped (Bug A edge case) covers
// the "log and drop" branch: adapter emits EventToolComplete keyed by
// meta.task_id but no tool_call row carries that task_id — the launch
// was never persisted. Triage must not error and must not create a
// phantom completion row.
func TestTaskUpdatedUnmatchedMetaTaskIDIsDropped(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	completeMeta, _ := json.Marshal(map[string]any{
		"is_background": true,
		"task_id":       "missing-task",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: "",
		Meta: completeMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("unmatched task_id completion: %v", err)
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected no rows for unmatched meta.task_id, got %+v", items)
	}
}
