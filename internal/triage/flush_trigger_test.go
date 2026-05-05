package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// flush_trigger_test.go covers the wire-event side of the flush
// queue: handleToolStart's trigger detection, the subagent
// ParentToolUseID filter, the per-round once-only invariant, and the
// multi-round re-fire on round transitions. flush_queue_test.go
// covers the storage primitives the trigger uses; this file tests the
// EventToolStart routing that drives it.

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

func TestHandleToolStart_FiresFlushTrigger_OnFirstNonSubagentTool(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	rec := &recordingDispatcher{}
	router.SetFlushDispatcher(rec.dispatch)

	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "first"))
	router.RegisterQueueItem("t1", makeQueueItem("queue:1", "second"))
	router.setOpenRound("t1", "round-A")

	if err := router.Handle(makeToolStartEvent("t1", "tool-1")); err != nil {
		t.Fatalf("handle tool start: %v", err)
	}

	calls := rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("dispatcher calls: got %d, want 1", len(calls))
	}
	if calls[0].ThreadID != "t1" || len(calls[0].Items) != 2 {
		t.Errorf("dispatch[0]: thread=%q items=%d, want t1 / 2", calls[0].ThreadID, len(calls[0].Items))
	}
	if router.HasQueuedFlushItems("t1") {
		t.Errorf("queue should be empty after fire")
	}
}

func TestHandleToolStart_SubagentToolUse_DoesNotFireTrigger(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	rec := &recordingDispatcher{}
	router.SetFlushDispatcher(rec.dispatch)

	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "x"))
	router.setOpenRound("t1", "round-A")

	subagentEvt := makeToolStartEvent("t1", "tool-sub")
	subagentEvt.ParentToolUseID = "parent-task-1"
	if err := router.Handle(subagentEvt); err != nil {
		t.Fatalf("handle subagent tool start: %v", err)
	}

	if len(rec.snapshot()) != 0 {
		t.Errorf("subagent tool_use fired flush trigger; should have skipped")
	}
	if !router.HasQueuedFlushItems("t1") {
		t.Errorf("queue drained on subagent tool_use; should have been preserved")
	}
}

func TestHandleToolStart_NoOpenRound_DoesNotFireTrigger(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	rec := &recordingDispatcher{}
	router.SetFlushDispatcher(rec.dispatch)

	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "x"))
	// Deliberately NOT calling setOpenRound — the trigger has no
	// round id to anchor "fired this round" against.

	if err := router.Handle(makeToolStartEvent("t1", "tool-1")); err != nil {
		t.Fatalf("handle tool start: %v", err)
	}

	if len(rec.snapshot()) != 0 {
		t.Errorf("trigger fired without an open round")
	}
	if !router.HasQueuedFlushItems("t1") {
		t.Errorf("queue drained without an open round")
	}
}

// TestHandleToolStart_SecondToolSameRound_FiresWhenNewItemsArrived
// is the regression guard for the per-round suppression bug. The
// previous design fired AT MOST ONCE per round; if the user queued
// a second message within the same wire round, the next top-level
// tool_use silently no-op'd and the message stayed in Zone 1 until
// the round closed. Reported with a real reproduction: subagents
// flushed test1 at the post-subagent seam, the user queued test2
// during the next bash sequence, and the bash's tool_start was
// suppressed. Now the trigger drains whenever the queue has items
// and a top-level tool_use lands in an open round.
func TestHandleToolStart_SecondToolSameRound_FiresWhenNewItemsArrived(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	rec := &recordingDispatcher{}
	router.SetFlushDispatcher(rec.dispatch)

	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "first"))
	router.setOpenRound("t1", "round-A")

	if err := router.Handle(makeToolStartEvent("t1", "tool-1")); err != nil {
		t.Fatalf("handle first tool start: %v", err)
	}
	router.RegisterQueueItem("t1", makeQueueItem("queue:1", "second"))
	if err := router.Handle(makeToolStartEvent("t1", "tool-2")); err != nil {
		t.Fatalf("handle second tool start: %v", err)
	}

	calls := rec.snapshot()
	if len(calls) != 2 {
		t.Fatalf("dispatcher calls: got %d, want 2 (each top-level tool_use with new queued items must drain)", len(calls))
	}
	if calls[0].Items[0].ID != "queue:0" {
		t.Errorf("first dispatch: got %q, want queue:0", calls[0].Items[0].ID)
	}
	if calls[1].Items[0].ID != "queue:1" {
		t.Errorf("second dispatch: got %q, want queue:1 — message queued mid-round was locked out", calls[1].Items[0].ID)
	}
	if router.HasQueuedFlushItems("t1") {
		t.Errorf("queue should be empty after both drains")
	}
}

// TestHandleToolStart_SecondToolSameRound_NoNewItemsNoOp pins that
// the queue-empty check still suppresses redundant dispatches. Two
// tool_starts back-to-back with no intervening register: the second
// finds the queue empty and no-ops, even with the per-round marker
// gone.
func TestHandleToolStart_SecondToolSameRound_NoNewItemsNoOp(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	rec := &recordingDispatcher{}
	router.SetFlushDispatcher(rec.dispatch)

	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "first"))
	router.setOpenRound("t1", "round-A")

	if err := router.Handle(makeToolStartEvent("t1", "tool-1")); err != nil {
		t.Fatalf("handle first tool start: %v", err)
	}
	if err := router.Handle(makeToolStartEvent("t1", "tool-2")); err != nil {
		t.Fatalf("handle second tool start: %v", err)
	}

	calls := rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("dispatcher calls: got %d, want 1 (queue is empty after first drain)", len(calls))
	}
}

func TestHandleToolStart_NewRoundFiresAgain(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	rec := &recordingDispatcher{}
	router.SetFlushDispatcher(rec.dispatch)

	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "first"))
	router.setOpenRound("t1", "round-A")
	if err := router.Handle(makeToolStartEvent("t1", "tool-A1")); err != nil {
		t.Fatalf("handle round-A tool: %v", err)
	}

	// Round-A complete → setOpenRound for the next round (the
	// multi-result-per-turn cascade path).
	router.setOpenRound("t1", "round-B")
	router.RegisterQueueItem("t1", makeQueueItem("queue:1", "second"))
	if err := router.Handle(makeToolStartEvent("t1", "tool-B1")); err != nil {
		t.Fatalf("handle round-B tool: %v", err)
	}

	calls := rec.snapshot()
	if len(calls) != 2 {
		t.Fatalf("dispatcher calls: got %d, want 2", len(calls))
	}
	if calls[0].Items[0].ID != "queue:0" || calls[1].Items[0].ID != "queue:1" {
		t.Errorf("dispatch ordering: got [%s, %s], want [queue:0, queue:1]",
			calls[0].Items[0].ID, calls[1].Items[0].ID)
	}
}

func TestHandleToolStart_EmptyQueue_NormalRoutingIntact(t *testing.T) {
	// When there's nothing queued, the trigger is a no-op and the
	// rest of handleToolStart (persistToolCallLaunch, trackToolPaths,
	// emitInline) must run unchanged. The persisted tool_call row is
	// the proxy assertion for "normal routing intact."
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	rec := &recordingDispatcher{}
	router.SetFlushDispatcher(rec.dispatch)

	router.setOpenRound("t1", "round-A")

	if err := router.Handle(makeToolStartEvent("t1", "tool-1")); err != nil {
		t.Fatalf("handle tool start: %v", err)
	}

	if len(rec.snapshot()) != 0 {
		t.Errorf("dispatcher fired with empty queue")
	}
	row, found, err := st.GetThreadItem("t1", "tool-1")
	if err != nil {
		t.Fatalf("get tool_call row: %v", err)
	}
	if !found {
		t.Errorf("tool_call row was not persisted — handleToolStart's normal routing was suppressed")
	}
	if row.Kind != itemKindToolCall {
		t.Errorf("tool_call row kind: got %q, want %q", row.Kind, itemKindToolCall)
	}
}

func TestHandleToolStart_DispatcherNilNotFatal(t *testing.T) {
	// Production wires the dispatcher at app startup; the brief
	// window before that (or any test that exercises only the
	// registration path) must not cause handleToolStart to error.
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	router.SetFlushDispatcher(nil)

	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "x"))
	router.setOpenRound("t1", "round-A")

	if err := router.Handle(makeToolStartEvent("t1", "tool-1")); err != nil {
		t.Fatalf("handle tool start with nil dispatcher: %v", err)
	}
	// Items remain queued — without a dispatcher there's no one to
	// receive the batch, so consuming would lose data.
	if !router.HasQueuedFlushItems("t1") {
		t.Errorf("queue drained without a dispatcher")
	}
}
