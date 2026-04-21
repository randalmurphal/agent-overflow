package triage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// firstItemByKind returns the first item of the given kind for a
// thread. Used by settle tests that need to reach the actual row id
// without coupling to the router's internal counter state.
func firstItemByKind(t *testing.T, st *store.Store, threadID, kind string) store.Item {
	t.Helper()
	items := findItemsByKind(t, st, threadID, kind)
	if len(items) == 0 {
		t.Fatalf("no %q items for thread %s", kind, threadID)
	}
	return items[0]
}

// TestInterruptQueueDrainsInArrivalOrder pins the FIFO contract on the
// interrupt queue. While a streaming text item is open, two background
// tool completions (A then B) arrive. The queue holds both, and on
// settle the persistence order MUST be A → B. If the drain switched to
// LIFO (or a map iteration, etc.), tool completions would land in the
// timeline out of order relative to when the provider emitted them —
// users would see the second-finished background task appear before the
// first-finished.
func TestInterruptQueueDrainsInArrivalOrder(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Seed two background tool_call launches BEFORE streaming text opens —
	// handleToolStart calls settleStreamingScope, which would close a
	// streaming block if one is already open.
	startMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Bash",
		"is_background": true,
		"input":         map[string]any{"command": "bg"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-A",
		ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start A: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-B",
		ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start B: %v", err)
	}

	// Now open a streaming text item so the interrupt queue engages.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   "streaming ",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}

	// Sanity: no background_done rows should exist yet (text is streaming).
	doneBefore := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(doneBefore) != 0 {
		t.Fatalf("background_done rows appeared before queue drain: %d", len(doneBefore))
	}

	// Post-refactor: sibling-row creation fires from
	// EventBackgroundTaskTerminal (task lifecycle), not EventToolComplete
	// (tool-lifecycle placeholder). The terminal event is what queues.
	terminalMetaA, _ := json.Marshal(map[string]any{
		"task_id":     "tsk-A",
		"tool_use_id": "bg-A",
		"status":      "completed",
		"exit_code":   0,
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "bg-A",
		Meta: terminalMetaA, Content: "body A", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("terminal A: %v", err)
	}
	terminalMetaB, _ := json.Marshal(map[string]any{
		"task_id":     "tsk-B",
		"tool_use_id": "bg-B",
		"status":      "completed",
		"exit_code":   0,
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "bg-B",
		Meta: terminalMetaB, Content: "body B", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("terminal B: %v", err)
	}

	// While streaming is still open, neither completion has been persisted.
	doneQueued := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(doneQueued) != 0 {
		t.Fatalf("background_done rows should still be queued, got %d", len(doneQueued))
	}

	// Settle streaming by stopping the content block; the queue must drain.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventContentBlockStop,
		ThreadID:  "t1",
		Meta:      json.RawMessage(`{"blockType":"text"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("content block stop: %v", err)
	}

	drained := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(drained) != 2 {
		t.Fatalf("expected 2 drained background_done items, got %d", len(drained))
	}

	// Persistence order is the observable fact we care about. ListItems
	// orders by (turn_index, item_index), and item_index is monotonically
	// assigned at persist time — so an item_index < tells us A landed before B.
	var aIdx, bIdx = -1, -1
	for _, it := range drained {
		switch it.CompletionOf {
		case "bg-A":
			aIdx = it.ItemIndex
		case "bg-B":
			bIdx = it.ItemIndex
		}
	}
	if aIdx < 0 || bIdx < 0 {
		t.Fatalf("missing completion rows: aIdx=%d bIdx=%d items=%+v", aIdx, bIdx, drained)
	}
	if aIdx >= bIdx {
		t.Fatalf("arrival order violated: A item_index=%d must be < B item_index=%d", aIdx, bIdx)
	}

	// Mirror check on the emission stream: the order in which
	// provider:item_upsert events fire for the two completions must also
	// be A → B (the frontend reconciler replays emissions in order).
	upsertOrder := []string{}
	for _, e := range *emissions {
		if e.eventName != "provider:item_upsert" {
			continue
		}
		item, ok := e.data.(store.Item)
		if !ok {
			continue
		}
		if item.Kind != itemKindBackgroundDone {
			continue
		}
		upsertOrder = append(upsertOrder, item.CompletionOf)
	}
	if len(upsertOrder) < 2 {
		t.Fatalf("expected 2 background_done upserts, got %+v", upsertOrder)
	}
	if upsertOrder[0] != "bg-A" ||
		upsertOrder[1] != "bg-B" {
		t.Fatalf("upsert arrival order violated: got %+v, want [bg-A, bg-B]", upsertOrder)
	}
}

// TestSettleNonStreamingRowStillDrainsQueue pins a subtle invariant in
// settleStreamingText / settleStreamingThinking: once the streaming
// counter has been decremented inside the lock, the interrupt-queue
// drain MUST run even if the store lookup finds the row in a
// non-streaming state. Without the drain-after-decrement path, a late
// settle that raced with an external status flip would leak every
// queued completion for that thread.
//
// Scenario:
//   1. Streaming text opens for a turn (counter = 1, row inserted as streaming).
//   2. A background tool completes and queues behind the streaming block.
//   3. Before the block closes, the row's status is flipped OUT of streaming
//      (simulating a crash-flip handler that got in first, or any path that
//      mutated the row's status while the block was still tracked active).
//   4. Content block stop fires: settle decrements the counter to 0 but the
//      row is now non-streaming. The drain still has to run — otherwise the
//      queued completion sits in `interruptQueue[threadID]` forever.
func TestSettleNonStreamingRowStillDrainsQueue(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// 1. Launch a background tool BEFORE streaming text opens. handleToolStart
	//    calls settleStreamingScope, which would close the streaming block if
	//    one were already open — so order matters.
	startMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Bash",
		"is_background": true,
		"input":         map[string]any{"command": "bg"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-x",
		ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start bg: %v", err)
	}

	// 2. Stream a text delta. First delta inserts the row with status=streaming
	//    and bumps streamingItemCounts[threadID] to 1.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   "streaming ",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}

	// 3. Fire the background task terminal while text is streaming —
	//    sibling-row upsert queues behind the active block instead of
	//    persisting inline. (Post-refactor: sibling creation moved from
	//    EventToolComplete to EventBackgroundTaskTerminal.)
	terminalMeta, _ := json.Marshal(map[string]any{
		"task_id":     "tsk-x",
		"tool_use_id": "bg-x",
		"status":      "completed",
		"exit_code":   0,
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "bg-x",
		Meta: terminalMeta, Content: "done", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("bg terminal: %v", err)
	}

	doneQueued := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(doneQueued) != 0 {
		t.Fatalf("precondition: background_done should still be queued behind streaming text, got %d", len(doneQueued))
	}

	// 3. Flip the row OUT of streaming externally. In production this can
	//    happen via the fatal-error crash-flip path, which transitions
	//    every streaming/running row to errored before the content block
	//    stop arrives. We reach in with UpdateItemStatus to mimic the
	//    end state regardless of which path produced it. Look up the
	//    actual item id (text:<turn>:<segmentIndex>) rather than
	//    hard-coding it — the segment counter's value is internal to the
	//    router and the test shouldn't be coupled to it.
	textRow := firstItemByKind(t, st, "t1", "assistant_text")
	if err := st.UpdateItemStatus(textRow.ID, "errored", "streaming — interrupted", "", time.Now().UnixMilli()); err != nil {
		t.Fatalf("flip row status out of streaming: %v", err)
	}

	// 4. Fire content block stop. settleStreamingText decrements the
	//    counter to 0, sees the row is non-streaming, and MUST still
	//    drain the queued background completion.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventContentBlockStop,
		ThreadID:  "t1",
		Meta:      json.RawMessage(`{"blockType":"text"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("content block stop: %v", err)
	}

	drained := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(drained) != 1 {
		t.Fatalf("drain-after-non-streaming: expected 1 drained background_done row, got %d", len(drained))
	}
	if drained[0].CompletionOf != "bg-x" {
		t.Fatalf("drained row completion_of = %q, want bg-x", drained[0].CompletionOf)
	}
}

// TestSettleStreamingThinkingNonStreamingRowStillDrainsQueue mirrors the
// text-block invariant on the thinking path. Same failure mode (drain
// skipped when the store lookup returns a non-streaming row) would
// starve the queue identically — the thinking handler owns its own
// defer now, and this test guards the wiring.
func TestSettleStreamingThinkingNonStreamingRowStillDrainsQueue(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// 1. Launch a background tool BEFORE streaming thinking opens — order
	//    matters for the same reason as the text variant: handleToolStart
	//    would otherwise settle an already-open thinking block.
	startMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Bash",
		"is_background": true,
		"input":         map[string]any{"command": "bg"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-y",
		ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start bg: %v", err)
	}

	// 2. Stream a thinking delta. First delta inserts the row with
	//    status=streaming and bumps streamingItemCounts[threadID] to 1.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventThinking,
		ThreadID:  "t1",
		Content:   "pondering ",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("thinking delta: %v", err)
	}

	// 3. Fire the task terminal — queues behind the thinking block.
	//    (Post-refactor: sibling-row upsert moved from EventToolComplete
	//    to EventBackgroundTaskTerminal.)
	terminalMeta, _ := json.Marshal(map[string]any{
		"task_id":     "tsk-y",
		"tool_use_id": "bg-y",
		"status":      "completed",
		"exit_code":   0,
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "bg-y",
		Meta: terminalMeta, Content: "done", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("bg terminal: %v", err)
	}
	doneQueued := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(doneQueued) != 0 {
		t.Fatalf("precondition: background_done should be queued, got %d", len(doneQueued))
	}

	// 3. Flip the thinking row out of streaming externally.
	thinkRow := firstItemByKind(t, st, "t1", "thinking")
	if err := st.UpdateItemStatus(thinkRow.ID, "errored", "pondering — interrupted", thinkRow.PayloadID, time.Now().UnixMilli()); err != nil {
		t.Fatalf("flip row status: %v", err)
	}

	// 4. Content block stop must still drain despite the non-streaming row.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventContentBlockStop,
		ThreadID:  "t1",
		Meta:      json.RawMessage(`{"blockType":"thinking"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("content block stop: %v", err)
	}

	drained := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(drained) != 1 {
		t.Fatalf("drain-after-non-streaming: expected 1 drained background_done row, got %d", len(drained))
	}
}

// TestMarkUserInterruptRefreshesHighlightedContent guards against the
// regression where interrupted/stopped streaming rows carried stale HTML:
// settleStreamingText mutated item.Summary (" — stopped") but persistItem
// only re-rendered when HighlightedContent was empty, so the DOM showed
// pre-suffix markup next to post-suffix text.
func TestMarkUserInterruptRefreshesHighlightedContent(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1",
		Content: "**hello**", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}

	// Pre-interrupt: streaming row carries rendered markdown in
	// HighlightedContent with no "— stopped" suffix.
	pre := firstItemByKind(t, st, "t1", "assistant_text")
	if pre.HighlightedContent == "" {
		t.Fatalf("pre-interrupt HighlightedContent should be populated, got empty")
	}
	if strings.Contains(pre.HighlightedContent, "stopped") {
		t.Fatalf("pre-interrupt HighlightedContent should not contain 'stopped': %q", pre.HighlightedContent)
	}

	if _, err := router.MarkUserInterrupt("t1"); err != nil {
		t.Fatalf("mark user interrupt: %v", err)
	}

	post := firstItemByKind(t, st, "t1", "assistant_text")
	if !strings.Contains(post.Summary, "— stopped") {
		t.Fatalf("post-interrupt Summary should contain '— stopped', got %q", post.Summary)
	}
	if !strings.Contains(post.HighlightedContent, "— stopped") {
		t.Fatalf("post-interrupt HighlightedContent should contain '— stopped', got %q", post.HighlightedContent)
	}
}
