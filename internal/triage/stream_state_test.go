package triage

import (
	"context"
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
		"source":      "task_output",
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
		"source":      "task_output",
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
	// Async settle on the read-loop hot path. Wait so the queue drain
	// runs before the assertion (finishSettle → drainInterruptQueueIfIdle).
	router.WaitForPendingSettles()

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
	// provider:item_event events fire for the two completions must also
	// be A → B (the frontend reconciler replays emissions in order).
	upsertOrder := []string{}
	for _, item := range filterItemEventUpserts(emissions.snapshot()) {
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
//  1. Streaming text opens for a turn (counter = 1, row inserted as streaming).
//  2. A background tool completes and queues behind the streaming block.
//  3. Before the block closes, the row's status is flipped OUT of streaming
//     (simulating a crash-flip handler that got in first, or any path that
//     mutated the row's status while the block was still tracked active).
//  4. Content block stop fires: settle decrements the counter to 0 but the
//     row is now non-streaming. The drain still has to run — otherwise the
//     queued completion sits in `interruptQueue[threadID]` forever.
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
		"source":      "task_output",
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
	//    stop arrives. We reach in with UpsertItem to mimic the end state
	//    regardless of which path produced it. Look up the actual item
	//    id (text:<turn>:<segmentIndex>) rather than hard-coding it —
	//    the segment counter's value is internal to the router and the
	//    test shouldn't be coupled to it.
	textRow := firstItemByKind(t, st, "t1", "assistant_text")
	textRow.Status = "errored"
	textRow.Summary = "streaming — interrupted"
	textRow.PayloadID = ""
	textRow.UpdatedAt = time.Now().UnixMilli()
	if _, err := st.UpsertItem(textRow, nil); err != nil {
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
	router.WaitForPendingSettles()

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
		"source":      "task_output",
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

	// 3. Flip the thinking row out of streaming externally — same
	//    crash-flip mimic as the text variant above, via UpsertItem.
	thinkRow := firstItemByKind(t, st, "t1", "thinking")
	thinkRow.Status = "errored"
	thinkRow.Summary = "pondering — interrupted"
	thinkRow.UpdatedAt = time.Now().UnixMilli()
	if _, err := st.UpsertItem(thinkRow, nil); err != nil {
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
	router.WaitForPendingSettles()

	drained := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(drained) != 1 {
		t.Fatalf("drain-after-non-streaming: expected 1 drained background_done row, got %d", len(drained))
	}
}

// TestThinkingPersistedSummaryReflectsTailAcrossStreamingAndSettle
// pins two coupled invariants:
//
//  1. The persisted `items.summary` for a thinking row is bounded to
//     `thinkingPreviewRunes` characters of the END of the content,
//     not the head. The frontend's 3-line tail viewport relies on
//     this — a head-capped summary would show the BEGINNING of
//     reasoning instead of the conclusion on cold reload.
//  2. The tail is produced by the streaming-flush path
//     (`AppendItemSummaryTail`), so `settleStreamingThinking` does
//     NOT re-read `payloads.data` on the hot event path. The prior
//     settle-time payload read was perceptible as an
//     end-of-thinking freeze that queued subsequent tool/text events
//     behind one synchronous BLOB load + meta write.
//
// The thinking row is driven through TWO deltas to exercise the
// streaming-flush append (not just the first-delta seed). The
// pre-settle assertion makes invariant 2 explicit — the persisted
// summary must already be the tail before the content_block_stop
// fires.
func TestThinkingPersistedSummaryReflectsTailAcrossStreamingAndSettle(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	leadingMarker := "BEGIN-marker"
	trailingMarker := "END-marker"
	// Two deltas crossing the cap. Each is >= the cap on its own so
	// the post-cap tail is sourced entirely from the LAST delta —
	// guarantees we exercise tail truncation, not pre-cap concat.
	firstDelta := leadingMarker + strings.Repeat("x", thinkingPreviewRunes*2)
	secondDelta := strings.Repeat("y", thinkingPreviewRunes) + trailingMarker

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventThinking,
		ThreadID:  "t1",
		Content:   firstDelta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("first thinking delta: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventThinking,
		ThreadID:  "t1",
		Content:   secondDelta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("second thinking delta: %v", err)
	}
	if err := router.Wait(context.Background()); err != nil {
		t.Fatalf("wait flush (pre-settle): %v", err)
	}

	row := firstItemByKind(t, st, "t1", "thinking")
	if len([]rune(row.Summary)) != thinkingPreviewRunes {
		t.Fatalf("pre-settle summary length = %d runes, want %d (tail-cap from streaming flush)",
			len([]rune(row.Summary)), thinkingPreviewRunes)
	}
	if !strings.Contains(row.Summary, trailingMarker) {
		t.Fatalf("pre-settle summary should contain trailing marker %q (tail-cap from streaming flush), got %q", trailingMarker, row.Summary)
	}
	if strings.Contains(row.Summary, leadingMarker) {
		t.Fatalf("pre-settle summary should NOT contain leading marker %q (regression: head-cap leaked through), got %q", leadingMarker, row.Summary)
	}
	if strings.HasPrefix(row.Summary, "...") {
		t.Fatalf("pre-settle summary should not carry a leading ellipsis marker (frontend tail-clips visually), got %q", row.Summary)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventContentBlockStop,
		ThreadID:  "t1",
		Meta:      json.RawMessage(`{"blockType":"thinking"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("content block stop: %v", err)
	}
	if err := router.Wait(context.Background()); err != nil {
		t.Fatalf("wait flush (post-settle): %v", err)
	}

	settled := firstItemByKind(t, st, "t1", "thinking")
	if settled.Status != statusCompleted {
		t.Fatalf("status after settle = %q, want %q", settled.Status, statusCompleted)
	}
	if settled.Summary != row.Summary {
		t.Fatalf("settle path must not rewrite items.summary (would force a payloads.data read on the hot event path); pre-settle = %q, post-settle = %q",
			row.Summary, settled.Summary)
	}
}

// TestDrainInterruptQueueContinuesAfterPersistError pins the contract
// that one failing item in the interrupt queue MUST NOT starve the
// rest: drainInterruptQueue used to early-return on the first persist
// error, leaving every later queued item stranded in the already-
// deleted queue.
//
// Case 1 (three items, middle fails, forceErrored=false): the valid
// items before AND after the failing one both land; the error
// propagates.
//
// Case 2 (two items, first fails, forceErrored=true): the mutated
// status/summary on the second (forced-error) path still applies —
// a regression would reorder the branch so a failing first item
// bypasses the mutation for the second.
func TestDrainInterruptQueueContinuesAfterPersistError(t *testing.T) {
	t.Run("middle_failure_allows_later_items", func(t *testing.T) {
		router, st, _ := newTestRouter(t)
		createTestThread(t, st, "t1")
		insertToolCallItem(t, st, "t1", "launch-pre", "Bash pre", "Bash", statusRunning)
		insertToolCallItem(t, st, "t1", "launch-post", "Bash post", "Bash", statusRunning)

		router.mu.Lock()
		router.interruptQueue["t1"] = []queuedPersistence{
			validDrainCompletion("complete:launch-pre", "launch-pre", 10, 1),
			{item: store.Item{
				ID: "bad-kind", ThreadID: "t1", TurnIndex: 0, ItemIndex: 11,
				Kind: "not_a_valid_kind", // CHECK constraint violation
				Role: "assistant", Status: statusCompleted,
				Summary: "bad", CreatedAt: 2, UpdatedAt: 2,
			}},
			validDrainCompletion("complete:launch-post", "launch-post", 12, 3),
		}
		router.mu.Unlock()

		if err := router.drainInterruptQueue("t1", false); err == nil {
			t.Fatal("expected first persist error to propagate")
		}

		done := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
		got := map[string]bool{}
		for _, it := range done {
			got[it.ID] = true
		}
		for _, want := range []string{"complete:launch-pre", "complete:launch-post"} {
			if !got[want] {
				t.Errorf("drain skipped %q — %v", want, got)
			}
		}

		router.mu.Lock()
		remaining := len(router.interruptQueue["t1"])
		router.mu.Unlock()
		if remaining != 0 {
			t.Errorf("interruptQueue residue: %d", remaining)
		}
	})

	t.Run("force_errored_mutation_applies_past_failure", func(t *testing.T) {
		router, st, _ := newTestRouter(t)
		createTestThread(t, st, "t1")
		insertToolCallItem(t, st, "t1", "launch-ok", "Bash", "Bash", statusRunning)

		valid := validDrainCompletion("complete:launch-ok", "launch-ok", 11, 2)
		valid.item.Status = statusCompleted // drain must flip this to errored
		router.mu.Lock()
		router.interruptQueue["t1"] = []queuedPersistence{
			{item: store.Item{
				ID: "bad-kind", ThreadID: "t1", TurnIndex: 0, ItemIndex: 10,
				Kind: "not_a_valid_kind",
				Role: "assistant", Status: statusCompleted,
				Summary: "bad", CreatedAt: 1, UpdatedAt: 1,
			}},
			valid,
		}
		router.mu.Unlock()

		if err := router.drainInterruptQueue("t1", true); err == nil {
			t.Fatal("expected first persist error to propagate")
		}

		done := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
		if len(done) != 1 || done[0].ID != "complete:launch-ok" {
			t.Fatalf("expected only complete:launch-ok, got %+v", done)
		}
		if done[0].Status != statusErrored {
			t.Errorf("forceErrored not applied to later item: status=%q, want %q", done[0].Status, statusErrored)
		}
		if !strings.Contains(done[0].Summary, "— interrupted") {
			t.Errorf("forceErrored suffix missing: summary=%q", done[0].Summary)
		}
	})
}

// validDrainCompletion builds a production-shaped background completion
// suitable for direct queue seeding. Keeps the two drain sub-tests from
// diverging on incidental Item fields.
func validDrainCompletion(id, launchID string, itemIndex int, createdAt int64) queuedPersistence {
	return queuedPersistence{item: store.Item{
		ID:           id,
		ThreadID:     "t1",
		TurnIndex:    0,
		ItemIndex:    itemIndex,
		Kind:         itemKindBackgroundDone,
		Role:         "assistant",
		Status:       statusCompleted,
		Summary:      id,
		CompletionOf: launchID,
		IsBackground: true,
		CreatedAt:    createdAt,
		UpdatedAt:    createdAt,
	}}
}

// TestContentBlockStopDoesNotBlockHandle pins the async-settle contract:
// content-block-stop must NOT wait for SQLite persist before returning.
// Without the async refactor the provider read-loop sat on
// flushStreamingItem → GetThreadItem → persistItem (3 transactions,
// 12-14 SQL statements) on every block end; the user-visible effect
// was a multi-frame freeze between a thinking block ending and the
// next agent output streaming in.
//
// Observable signal: immediately after Handle returns, the streaming
// row's status MAY still be "streaming" (the goroutine is in flight);
// after WaitForPendingSettles it MUST be "completed". A failing-to-be-
// async path settles synchronously and the row is "completed" before
// the wait. We assert the WaitForPendingSettles is necessary by
// observing that the settle goroutine is tracked — i.e., after Handle
// returns, the goroutine count is non-zero until the wait drains it.
//
// We can't directly observe goroutine count, so we use the persistent
// state as a proxy: between Handle returning and WaitForPendingSettles
// returning, the row's status MUST transition from streaming to
// completed (i.e., persistItem ran). With the sync path, both reads
// would see "completed" because the persist already happened inside
// Handle. With async, the first read can race the settle goroutine —
// so we instead pin the contract that WaitForPendingSettles is
// REQUIRED to observe a completed row: assert that Handle alone
// doesn't guarantee completion.
func TestContentBlockStopDoesNotBlockHandle(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   "hello",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 || items[0].Status != "streaming" {
		t.Fatalf("setup: expected 1 streaming row, got %+v", items)
	}
	itemID := items[0].ID

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventContentBlockStop,
		ThreadID:  "t1",
		Meta:      json.RawMessage(`{"blockType":"text"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("content block stop: %v", err)
	}

	// The contract: WaitForPendingSettles must be called to observe the
	// settled row. After the wait, status is "completed".
	router.WaitForPendingSettles()

	settled, found, err := st.GetThreadItem("t1", itemID)
	if err != nil || !found {
		t.Fatalf("get after wait: found=%v err=%v", found, err)
	}
	if settled.Status != statusCompleted {
		t.Fatalf("after WaitForPendingSettles: status=%q, want %q", settled.Status, statusCompleted)
	}
}

// TestSettleTurnStreamingAwaitsAllScopes pins the synchronous barrier
// in settleTurnStreaming: when a turn ends with multiple active
// streaming scopes (e.g. an interrupt while a top-level text block
// AND a subagent text block are both streaming), settleTurnStreaming
// must wait for every per-scope goroutine before returning so the
// turn-row UPDATE downstream sequences correctly. A failing-to-await
// settleTurnStreaming would race the turn-row commit against
// in-flight streaming-item commits.
func TestSettleTurnStreamingAwaitsAllScopes(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	// Open two streaming scopes: top-level + parent-tool-use-scoped.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   "top ",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("top delta: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:            provider.EventTextDelta,
		ThreadID:        "t1",
		ParentToolUseID: "sub-1",
		Content:         "sub ",
		Timestamp:       time.Now(),
	}); err != nil {
		t.Fatalf("sub delta: %v", err)
	}

	streaming, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list before complete: %v", err)
	}
	streamingIDs := []string{}
	for _, it := range streaming {
		if it.Status == statusStreaming {
			streamingIDs = append(streamingIDs, it.ID)
		}
	}
	if len(streamingIDs) != 2 {
		t.Fatalf("setup: expected 2 streaming rows, got %d (%+v)", len(streamingIDs), streaming)
	}

	// Fire turn complete: settleTurnStreaming MUST wait for both
	// scopes' settle goroutines before returning. After the Handle call,
	// both rows must be completed — no WaitForPendingSettles required.
	if err := router.Handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     "t1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}

	for _, id := range streamingIDs {
		row, found, err := st.GetThreadItem("t1", id)
		if err != nil || !found {
			t.Fatalf("get %s: found=%v err=%v", id, found, err)
		}
		if row.Status != statusCompleted {
			t.Errorf("after turn-complete: %s status=%q, want %q (settleTurnStreaming did not await goroutine)", id, row.Status, statusCompleted)
		}
	}
}

func TestProviderItemIDsSplitAssistantTextRows(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1", TurnID: "turn-1", ItemID: "msg-a",
		Content: "moving bottom.", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("first text delta: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1", TurnID: "turn-1", ItemID: "msg-b",
		Content: "I'll verify the next row.", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("second text delta: %v", err)
	}

	for _, itemID := range []string{"msg-a", "msg-b"} {
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventContentBlockStop, ThreadID: "t1", TurnID: "turn-1", ItemID: itemID,
			Meta: json.RawMessage(`{"blockType":"text"}`), Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("stop %s: %v", itemID, err)
		}
	}
	router.WaitForPendingSettles()

	textItems := findItemsByKind(t, st, "t1", itemKindAssistantText)
	if len(textItems) != 2 {
		t.Fatalf("assistant_text rows = %d, want 2: %+v", len(textItems), textItems)
	}
	if textItems[0].Summary != "moving bottom." {
		t.Fatalf("first summary = %q, want moving bottom.", textItems[0].Summary)
	}
	if textItems[1].Summary != "I'll verify the next row." {
		t.Fatalf("second summary = %q, want second provider item", textItems[1].Summary)
	}
	for _, item := range textItems {
		if strings.Contains(item.Summary, "bottom.I'll") {
			t.Fatalf("provider item boundary collapsed into one row: %+v", item)
		}
	}
}

func TestProviderItemCompletionReplacesStreamedAssistantText(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1", TurnID: "turn-1", ItemID: "msg-a",
		Content: "draft answer", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: "t1", TurnID: "turn-1", ItemID: "msg-a",
		Content: "final answer", ContentPresent: true,
		Meta: json.RawMessage(`{"blockType":"text"}`), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("content block stop: %v", err)
	}
	router.WaitForPendingSettles()

	item := firstItemByKind(t, st, "t1", itemKindAssistantText)
	if item.Summary != "final answer" {
		t.Fatalf("summary = %q, want final authoritative content", item.Summary)
	}
	if item.Status != statusCompleted {
		t.Fatalf("status = %q, want completed", item.Status)
	}
}

func TestProviderItemCompletionCanCreateCompletedAssistantText(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: "t1", TurnID: "turn-1", ItemID: "msg-a",
		Content: "final-only answer", ContentPresent: true,
		Meta: json.RawMessage(`{"blockType":"text"}`), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("content block stop: %v", err)
	}
	router.WaitForPendingSettles()

	item := firstItemByKind(t, st, "t1", itemKindAssistantText)
	if item.Summary != "final-only answer" {
		t.Fatalf("summary = %q, want final-only answer", item.Summary)
	}
	if item.Status != statusCompleted {
		t.Fatalf("status = %q, want completed", item.Status)
	}
}

func TestSubagentProviderItemCompletionUpdatesSettledTextWithoutTopLevelDuplicate(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	insertToolCallItem(t, st, "t1", "spawn-1", "Spawned reviewer", "spawn_agent", statusRunning)

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1", TurnID: "child-turn", ItemID: "child-msg",
		ParentToolUseID: "spawn-1", Content: "draft child text", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("child text delta: %v", err)
	}
	if err := router.settleStreamingScope("t1", "spawn-1"); err != nil {
		t.Fatalf("settle scope: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: "t1", TurnID: "child-turn", ItemID: "child-msg",
		ParentToolUseID: "spawn-1", Content: "final child text", ContentPresent: true,
		Meta: json.RawMessage(`{"blockType":"text"}`), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("late child content block stop: %v", err)
	}
	router.WaitForPendingSettles()

	textItems := findItemsByKind(t, st, "t1", itemKindAssistantText)
	if len(textItems) != 1 {
		t.Fatalf("assistant_text rows = %d, want 1: %+v", len(textItems), textItems)
	}
	item := textItems[0]
	if item.ParentID != "spawn-1" {
		t.Fatalf("parent_id = %q, want spawn-1", item.ParentID)
	}
	if item.Summary != "final child text" {
		t.Fatalf("summary = %q, want final child text", item.Summary)
	}
	if strings.TrimSpace(readProviderItemIDFromMeta(json.RawMessage(item.Meta))) != "child-msg" {
		t.Fatalf("meta.provider_item_id = %q, want child-msg (meta=%s)", readProviderItemIDFromMeta(json.RawMessage(item.Meta)), item.Meta)
	}
}

func TestSubagentProviderItemCompletionUsesSpawnTurnIndex(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	insertToolCallItem(t, st, "t1", "spawn-1", "Spawned reviewer", "spawn_agent", statusRunning)

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("new parent turn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: "t1", TurnID: "child-turn", ItemID: "child-msg",
		ParentToolUseID: "spawn-1", Content: "final-only child text", ContentPresent: true,
		Meta: json.RawMessage(`{"blockType":"text"}`), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("child content block stop: %v", err)
	}

	item := firstItemByKind(t, st, "t1", itemKindAssistantText)
	if item.TurnIndex != 0 {
		t.Fatalf("turn_index = %d, want spawn turn 0", item.TurnIndex)
	}
	if item.ParentID != "spawn-1" {
		t.Fatalf("parent_id = %q, want spawn-1", item.ParentID)
	}
}

func TestProviderItemCompletionCanReplaceAssistantTextWithEmptyFinal(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1", TurnID: "turn-1", ItemID: "msg-a",
		Content: "draft answer", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: "t1", TurnID: "turn-1", ItemID: "msg-a",
		Content: "", ContentPresent: true,
		Meta: json.RawMessage(`{"blockType":"text"}`), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("content block stop: %v", err)
	}
	router.WaitForPendingSettles()

	item := firstItemByKind(t, st, "t1", itemKindAssistantText)
	if item.Summary != "" {
		t.Fatalf("summary = %q, want empty authoritative final content", item.Summary)
	}
}

func TestProviderItemIDsSplitThinkingRows(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventThinking, ThreadID: "t1", TurnID: "turn-1", ItemID: "reason-a",
		Content: "first thought", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("first thinking delta: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventThinking, ThreadID: "t1", TurnID: "turn-1", ItemID: "reason-b",
		Content: "second thought", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("second thinking delta: %v", err)
	}

	for _, itemID := range []string{"reason-a", "reason-b"} {
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventContentBlockStop, ThreadID: "t1", TurnID: "turn-1", ItemID: itemID,
			Meta: json.RawMessage(`{"blockType":"thinking"}`), Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("stop %s: %v", itemID, err)
		}
	}
	router.WaitForPendingSettles()

	thinkingItems := findItemsByKind(t, st, "t1", itemKindThinking)
	if len(thinkingItems) != 2 {
		t.Fatalf("thinking rows = %d, want 2: %+v", len(thinkingItems), thinkingItems)
	}
	if thinkingItems[0].Summary != "first thought" {
		t.Fatalf("first thinking summary = %q, want first thought", thinkingItems[0].Summary)
	}
	if thinkingItems[1].Summary != "second thought" {
		t.Fatalf("second thinking summary = %q, want second thought", thinkingItems[1].Summary)
	}
}

func TestProviderItemCompletionCanCreateCompletedThinking(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: "t1", TurnID: "turn-1", ItemID: "reason-a",
		Content: "final-only thought", ContentPresent: true,
		Meta: json.RawMessage(`{"blockType":"thinking"}`), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("content block stop: %v", err)
	}
	router.WaitForPendingSettles()

	item := firstItemByKind(t, st, "t1", itemKindThinking)
	if item.Summary != "final-only thought" {
		t.Fatalf("summary = %q, want final-only thought", item.Summary)
	}
	data, err := st.GetPayloadData(item.PayloadID)
	if err != nil {
		t.Fatalf("get payload: %v", err)
	}
	if string(data) != "final-only thought" {
		t.Fatalf("payload = %q, want final-only thought", data)
	}
}

func TestSubagentProviderItemCompletionUpdatesSettledThinkingWithoutTopLevelDuplicate(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	insertToolCallItem(t, st, "t1", "spawn-1", "Spawned reviewer", "spawn_agent", statusRunning)

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventThinking, ThreadID: "t1", TurnID: "child-turn", ItemID: "child-reasoning",
		ParentToolUseID: "spawn-1", Content: "draft thought", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("child thinking delta: %v", err)
	}
	if err := router.settleStreamingScope("t1", "spawn-1"); err != nil {
		t.Fatalf("settle scope: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: "t1", TurnID: "child-turn", ItemID: "child-reasoning",
		ParentToolUseID: "spawn-1", Content: "final thought", ContentPresent: true,
		Meta: json.RawMessage(`{"blockType":"thinking"}`), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("late child thinking stop: %v", err)
	}
	router.WaitForPendingSettles()

	thinkingItems := findItemsByKind(t, st, "t1", itemKindThinking)
	if len(thinkingItems) != 1 {
		t.Fatalf("thinking rows = %d, want 1: %+v", len(thinkingItems), thinkingItems)
	}
	item := thinkingItems[0]
	if item.ParentID != "spawn-1" {
		t.Fatalf("parent_id = %q, want spawn-1", item.ParentID)
	}
	if item.Summary != "final thought" {
		t.Fatalf("summary = %q, want final thought", item.Summary)
	}
	data, err := st.GetPayloadData(item.PayloadID)
	if err != nil {
		t.Fatalf("get payload: %v", err)
	}
	if string(data) != "final thought" {
		t.Fatalf("payload = %q, want final thought", data)
	}
}

func TestProviderItemCompletionCanReplaceThinkingWithEmptyFinal(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventThinking, ThreadID: "t1", TurnID: "turn-1", ItemID: "reason-a",
		Content: "draft thought", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("thinking delta: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: "t1", TurnID: "turn-1", ItemID: "reason-a",
		Content: "", ContentPresent: true,
		Meta: json.RawMessage(`{"blockType":"thinking"}`), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("content block stop: %v", err)
	}
	router.WaitForPendingSettles()

	item := firstItemByKind(t, st, "t1", itemKindThinking)
	if item.Summary != "" {
		t.Fatalf("summary = %q, want empty authoritative final content", item.Summary)
	}
	data, err := st.GetPayloadData(item.PayloadID)
	if err != nil {
		t.Fatalf("get payload: %v", err)
	}
	if string(data) != "" {
		t.Fatalf("payload = %q, want empty authoritative final content", data)
	}
}

// TestTextBlockStartStreamsFirstChunkAsDelta pins the streaming-animation
// contract for the first chunk of an assistant_text block. The first chunk
// used to be baked into the creation upsert's Summary, so the frontend
// per-item smoother seeded it as already-revealed and it snapped in as one
// block with no animation — most visible when the first chunk is large (a
// whole assistant-message text block emitted by parse_assistant, or the
// opening burst after a thinking block). Row creation must carry an EMPTY
// summary and ALL content must arrive as deltas so every chunk animates;
// SQLite still persists the content for history / mid-stream restore.
func TestTextBlockStartStreamsFirstChunkAsDelta(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	// A thinking block precedes the text (the reported "thinking -> output"
	// case), which also pushes the text row to a non-zero item_index. The
	// emitted creation upsert must carry that store-assigned index; emitting
	// the pre-persist struct would ship item_index 0 and mis-sort against the
	// thinking row (and feed a wrong position into the reveal gate).
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventThinking, ThreadID: "t1", Content: "reasoning about it", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("thinking delta: %v", err)
	}

	first := "This is the opening of the agent response, long enough to look like a snap."
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1", Content: first, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("first text delta: %v", err)
	}

	// The row must already be persisted with the first chunk — emitting an
	// empty creation upsert must NOT drop content from SQLite history.
	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	var textID, persistedSummary string
	var persistedItemIndex int
	for _, it := range items {
		if it.Kind == itemKindAssistantText {
			textID = it.ID
			persistedSummary = it.Summary
			persistedItemIndex = it.ItemIndex
			break
		}
	}
	if textID == "" {
		t.Fatalf("no assistant_text row persisted; items=%+v", items)
	}
	if persistedSummary != first {
		t.Fatalf("persisted summary = %q, want %q (history must keep the content)", persistedSummary, first)
	}
	if persistedItemIndex == 0 {
		t.Fatalf("expected the text row to follow the thinking row at item_index > 0, got 0")
	}

	second := " And this continues streaming."
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1", Content: second, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("second text delta: %v", err)
	}

	var events []ItemStreamEvent
	for _, e := range filterItemStreamEvents(emissions.snapshot()) {
		if (e.Item != nil && e.Item.ID == textID) || e.ItemID == textID {
			events = append(events, e)
		}
	}
	if len(events) < 3 {
		t.Fatalf("want >=3 item-stream events (creation upsert + 2 deltas), got %d: %+v", len(events), events)
	}
	// 1) Row creation upsert with NO content baked into the summary, and the
	//    store-assigned item_index (NOT the zero-value pre-persist struct) so
	//    it sorts after the thinking row and the reveal gate sees the right
	//    position.
	if events[0].Action != itemStreamActionUpsert {
		t.Fatalf("event[0].Action = %q, want upsert", events[0].Action)
	}
	if events[0].Item == nil || events[0].Item.Summary != "" {
		t.Fatalf("creation upsert Summary = %q, want empty (content must stream as deltas)",
			events[0].Item.Summary)
	}
	if events[0].Item.ItemIndex != persistedItemIndex {
		t.Fatalf("creation upsert item_index = %d, want %d (the store-assigned index)",
			events[0].Item.ItemIndex, persistedItemIndex)
	}
	// 2) The first chunk arrives as a delta so the smoother animates it.
	if events[1].Action != itemStreamActionDelta || events[1].Delta != first {
		t.Fatalf("event[1] = {action:%q delta:%q}, want {delta, %q}",
			events[1].Action, events[1].Delta, first)
	}
	// 3) The second chunk continues as a delta.
	if events[2].Action != itemStreamActionDelta || events[2].Delta != second {
		t.Fatalf("event[2] = {action:%q delta:%q}, want {delta, %q}",
			events[2].Action, events[2].Delta, second)
	}
}

func TestAssistantTextPayloadKeepsStreamingSummaryFull(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	first := strings.Repeat("a", 32*1024)
	second := strings.Repeat("b", 32*1024)
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   first,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("first text delta: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   second,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("second text delta: %v", err)
	}

	item := firstItemByKind(t, st, "t1", itemKindAssistantText)
	if item.PayloadID == "" || item.PayloadKind != itemKindAssistantText {
		t.Fatalf("assistant text payload link = (%q, %q), want assistant_text payload", item.PayloadID, item.PayloadKind)
	}
	if item.Summary != first+second {
		t.Fatalf("summary length/content mismatch: got %d bytes, want full assistant text", len(item.Summary))
	}
	data, err := st.GetPayloadData(item.PayloadID)
	if err != nil {
		t.Fatalf("get payload: %v", err)
	}
	if string(data) != first+second {
		t.Fatalf("payload length/content mismatch: got %d bytes, want full assistant text", len(data))
	}

	final := "final " + strings.Repeat("c", 32*1024)
	if err := router.Handle(provider.ProviderEvent{
		Kind:           provider.EventContentBlockStop,
		ThreadID:       "t1",
		Content:        final,
		ContentPresent: true,
		Meta:           json.RawMessage(`{"blockType":"text"}`),
		Timestamp:      time.Now(),
	}); err != nil {
		t.Fatalf("content block stop: %v", err)
	}
	router.WaitForPendingSettles()

	settled := firstItemByKind(t, st, "t1", itemKindAssistantText)
	if settled.Status != statusCompleted {
		t.Fatalf("status = %q, want completed", settled.Status)
	}
	if settled.Summary != final {
		t.Fatalf("settled summary length/content mismatch: got %d bytes, want authoritative final content", len(settled.Summary))
	}
	data, err = st.GetPayloadData(settled.PayloadID)
	if err != nil {
		t.Fatalf("get settled payload: %v", err)
	}
	if string(data) != final {
		t.Fatalf("settled payload = %d bytes, want authoritative final content", len(data))
	}
}

func TestAssistantTextPayloadIDsAreThreadScoped(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "thread-a")
	createTestThread(t, st, "thread-b")

	for _, tc := range []struct {
		threadID string
		content  string
	}{
		{threadID: "thread-a", content: "assistant body for A"},
		{threadID: "thread-b", content: "assistant body for B"},
	} {
		if err := router.Handle(provider.ProviderEvent{
			Kind:      provider.EventTextDelta,
			ThreadID:  tc.threadID,
			Content:   tc.content,
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("text delta for %s: %v", tc.threadID, err)
		}
	}

	itemA := firstItemByKind(t, st, "thread-a", itemKindAssistantText)
	itemB := firstItemByKind(t, st, "thread-b", itemKindAssistantText)
	if itemA.ID != itemB.ID {
		t.Fatalf("test setup expected matching item ids, got %q and %q", itemA.ID, itemB.ID)
	}
	if itemA.PayloadID == itemB.PayloadID {
		t.Fatalf("payload ids should be thread-scoped, both were %q", itemA.PayloadID)
	}

	dataA, err := st.GetPayloadData(itemA.PayloadID)
	if err != nil {
		t.Fatalf("payload A: %v", err)
	}
	dataB, err := st.GetPayloadData(itemB.PayloadID)
	if err != nil {
		t.Fatalf("payload B: %v", err)
	}
	if string(dataA) != "assistant body for A" {
		t.Fatalf("payload A = %q", dataA)
	}
	if string(dataB) != "assistant body for B" {
		t.Fatalf("payload B = %q", dataB)
	}
}
