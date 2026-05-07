package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// flush_trigger_test.go covers the wire-event side of the flush queue:
// provider lifecycle events define the best available safe boundary for
// draining queued user messages. Tool starts are blocking boundaries, not
// drain points; tool/background/turn completion drains only after no
// top-level foreground tool or live background task remains.

func makeToolStartEvent(threadID, itemID string) provider.ProviderEvent {
	meta, _ := json.Marshal(map[string]any{"toolName": "Bash"})
	return provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  threadID,
		ItemID:    itemID,
		ItemType:  "Bash",
		Meta:      meta,
		Timestamp: time.Now(),
	}
}

func makeToolCompleteEvent(threadID, itemID string) provider.ProviderEvent {
	meta, _ := json.Marshal(map[string]any{"toolName": "Bash"})
	return provider.ProviderEvent{
		Kind:      provider.EventToolComplete,
		ThreadID:  threadID,
		ItemID:    itemID,
		ItemType:  "Bash",
		Meta:      meta,
		Timestamp: time.Now(),
	}
}

func createCodexQueueTestThread(t *testing.T, st *store.Store, id string) {
	t.Helper()
	ensureTriageProject(t, st)
	now := time.Now().UnixMilli()
	if err := st.CreateThread(store.Thread{
		ID:            id,
		ProjectID:     triageTestProjectID,
		Title:         "Codex Queue Test",
		Provider:      "codex",
		WorkspacePath: "/tmp",
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("create codex thread: %v", err)
	}
}

func TestHandleToolStart_DoesNotFlushQueuedMessages(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	rec := &recordingDispatcher{}
	router.SetFlushDispatcher(rec.dispatch)

	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "first"))
	router.setOpenRound("t1", "round-A")

	if err := router.Handle(makeToolStartEvent("t1", "tool-1")); err != nil {
		t.Fatalf("handle tool start: %v", err)
	}

	if len(rec.snapshot()) != 0 {
		t.Fatalf("tool start flushed queued messages; starts are still inside the active model step")
	}
	if !router.HasQueuedFlushItems("t1") {
		t.Fatalf("queue drained on tool start")
	}
	row, found, err := st.GetThreadItem("t1", "tool-1")
	if err != nil {
		t.Fatalf("get tool_call row: %v", err)
	}
	if !found || row.Status != statusRunning {
		t.Fatalf("tool start row = %+v found=%v, want running tool_call", row, found)
	}
}

func TestHandleToolComplete_FlushesWhenNoBlockingWorkRemains(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	rec := &recordingDispatcher{}
	router.SetFlushDispatcher(rec.dispatch)

	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "first"))
	router.setOpenRound("t1", "round-A")

	if err := router.Handle(makeToolStartEvent("t1", "tool-1")); err != nil {
		t.Fatalf("handle tool start: %v", err)
	}
	if err := router.Handle(makeToolCompleteEvent("t1", "tool-1")); err != nil {
		t.Fatalf("handle tool complete: %v", err)
	}

	calls := rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("dispatcher calls: got %d, want 1", len(calls))
	}
	if calls[0].ThreadID != "t1" || len(calls[0].Items) != 1 || calls[0].Items[0].ID != "queue:0" {
		t.Fatalf("dispatch[0] = %+v, want t1 queue:0", calls[0])
	}
	if router.HasQueuedFlushItems("t1") {
		t.Fatalf("queue should be empty after completion boundary")
	}
}

func TestHandleToolComplete_CodexUnifiedExecCompletionFlushesBoundary(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexQueueTestThread(t, st, "t1")
	rec := &recordingDispatcher{}
	router.SetFlushDispatcher(rec.dispatch)
	router.setOpenRound("t1", "round-A")

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "cmd-1",
		ItemType:  "commandExecution",
		Meta:      buildUnifiedExecStartMeta(t, "pid-1", "echo ok"),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle unified exec start: %v", err)
	}
	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "after command"))

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolComplete,
		ThreadID:  "t1",
		ItemID:    "cmd-1",
		ItemType:  "commandExecution",
		Meta:      buildUnifiedExecCompleteMeta(t, "completed", "pid-1", "echo ok", 0),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle unified exec complete: %v", err)
	}

	calls := rec.snapshot()
	if len(calls) != 1 || len(calls[0].Items) != 1 || calls[0].Items[0].ID != "queue:0" {
		t.Fatalf("dispatcher calls = %+v, want one queue:0 drain after unified exec completion", calls)
	}
}

func TestHandleToolComplete_WaitsForActiveCodexUnifiedExec(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexQueueTestThread(t, st, "t1")
	rec := &recordingDispatcher{}
	router.SetFlushDispatcher(rec.dispatch)
	router.setOpenRound("t1", "round-A")

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "cmd-1",
		ItemType:  "commandExecution",
		Meta:      buildUnifiedExecStartMeta(t, "pid-1", "sleep 30"),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle unified exec start: %v", err)
	}
	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "after all work"))

	if err := router.Handle(makeToolStartEvent("t1", "tool-1")); err != nil {
		t.Fatalf("handle normal tool start: %v", err)
	}
	if err := router.Handle(makeToolCompleteEvent("t1", "tool-1")); err != nil {
		t.Fatalf("handle normal tool complete: %v", err)
	}
	if len(rec.snapshot()) != 0 {
		t.Fatalf("normal tool completion flushed while unified exec was still active")
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolComplete,
		ThreadID:  "t1",
		ItemID:    "cmd-1",
		ItemType:  "commandExecution",
		Meta:      buildUnifiedExecCompleteMeta(t, "completed", "pid-1", "sleep 30", 0),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle unified exec complete: %v", err)
	}
	if len(rec.snapshot()) != 1 {
		t.Fatalf("unified exec completion should release queued flush, got calls=%+v", rec.snapshot())
	}
}

func TestHandleToolComplete_WaitsForOtherTopLevelTools(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	rec := &recordingDispatcher{}
	router.SetFlushDispatcher(rec.dispatch)

	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "first"))
	router.setOpenRound("t1", "round-A")

	for _, id := range []string{"tool-1", "tool-2"} {
		if err := router.Handle(makeToolStartEvent("t1", id)); err != nil {
			t.Fatalf("handle tool start %s: %v", id, err)
		}
	}
	if err := router.Handle(makeToolCompleteEvent("t1", "tool-1")); err != nil {
		t.Fatalf("handle first tool complete: %v", err)
	}
	if len(rec.snapshot()) != 0 {
		t.Fatalf("first completion flushed while another top-level tool was still running")
	}
	if !router.HasQueuedFlushItems("t1") {
		t.Fatalf("queue drained before final tool completed")
	}

	if err := router.Handle(makeToolCompleteEvent("t1", "tool-2")); err != nil {
		t.Fatalf("handle second tool complete: %v", err)
	}
	calls := rec.snapshot()
	if len(calls) != 1 || len(calls[0].Items) != 1 || calls[0].Items[0].ID != "queue:0" {
		t.Fatalf("dispatcher calls = %+v, want one queue:0 drain after final completion", calls)
	}
}

func TestHandleSubagentToolComplete_WaitsForParentTool(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	rec := &recordingDispatcher{}
	router.SetFlushDispatcher(rec.dispatch)

	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "first"))
	router.setOpenRound("t1", "round-A")

	if err := router.Handle(makeToolStartEvent("t1", "parent-task")); err != nil {
		t.Fatalf("handle parent start: %v", err)
	}
	childStart := makeToolStartEvent("t1", "child-tool")
	childStart.ParentToolUseID = "parent-task"
	if err := router.Handle(childStart); err != nil {
		t.Fatalf("handle child start: %v", err)
	}
	childComplete := makeToolCompleteEvent("t1", "child-tool")
	childComplete.ParentToolUseID = "parent-task"
	if err := router.Handle(childComplete); err != nil {
		t.Fatalf("handle child complete: %v", err)
	}
	if len(rec.snapshot()) != 0 {
		t.Fatalf("child completion flushed while parent Task was still running")
	}

	if err := router.Handle(makeToolCompleteEvent("t1", "parent-task")); err != nil {
		t.Fatalf("handle parent complete: %v", err)
	}
	if len(rec.snapshot()) != 1 {
		t.Fatalf("parent completion should flush queued messages, got calls=%+v", rec.snapshot())
	}
}

func TestHandleBackgroundTaskTerminal_FlushesAfterHostExitHidesLiveTask(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	rec := &recordingDispatcher{}
	router.SetFlushDispatcher(rec.dispatch)

	router.setOpenRound("t1", "round-A")
	bgStart := makeToolStartEvent("t1", "bg-tool")
	bgStart.Meta = json.RawMessage(`{"toolName":"Bash","is_background":true,"task_id":"task-1"}`)
	if err := router.Handle(bgStart); err != nil {
		t.Fatalf("handle background start: %v", err)
	}
	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "after background"))

	bgComplete := makeToolCompleteEvent("t1", "bg-tool")
	bgComplete.Meta = json.RawMessage(`{"toolName":"Bash","is_background":true,"task_id":"task-1"}`)
	if err := router.Handle(bgComplete); err != nil {
		t.Fatalf("handle background placeholder complete: %v", err)
	}
	if len(rec.snapshot()) != 0 {
		t.Fatalf("placeholder completion flushed while background task was still live")
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventBackgroundTaskTerminal,
		ThreadID:  "t1",
		ItemID:    "bg-tool",
		Meta:      json.RawMessage(`{"task_id":"task-1","tool_use_id":"bg-tool","status":"completed","source":"task_updated"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle background terminal: %v", err)
	}

	calls := rec.snapshot()
	if len(calls) != 1 || len(calls[0].Items) != 1 || calls[0].Items[0].ID != "queue:0" {
		t.Fatalf("dispatcher calls = %+v, want one queue:0 drain after task_updated terminal", calls)
	}
}

func TestHandleTurnComplete_FlushesTextOnlyRound(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	rec := &recordingDispatcher{}
	router.SetFlushDispatcher(rec.dispatch)
	seedOpenTurn(t, router, st, "t1", 0)

	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "after text"))

	if err := router.Handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     "t1",
		TurnIndex:    0,
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("handle turn complete: %v", err)
	}

	calls := rec.snapshot()
	if len(calls) != 1 || len(calls[0].Items) != 1 || calls[0].Items[0].ID != "queue:0" {
		t.Fatalf("dispatcher calls = %+v, want one queue:0 drain on text-only turn completion", calls)
	}
}

func TestHandleToolComplete_DispatcherNilKeepsQueue(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	router.SetFlushDispatcher(nil)

	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "x"))
	router.setOpenRound("t1", "round-A")

	if err := router.Handle(makeToolStartEvent("t1", "tool-1")); err != nil {
		t.Fatalf("handle tool start: %v", err)
	}
	if err := router.Handle(makeToolCompleteEvent("t1", "tool-1")); err != nil {
		t.Fatalf("handle tool complete: %v", err)
	}
	if !router.HasQueuedFlushItems("t1") {
		t.Fatalf("queue drained without a dispatcher")
	}
}
