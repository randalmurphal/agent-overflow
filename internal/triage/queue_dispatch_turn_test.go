package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// TestHandleTurnStart_PendingSendTurnIndex_PreservesQueueDispatchedTurn
// pins the queue-dispatch fix: when a queued user message is dispatched
// and the provider echoes a wire init that carries no turn index
// (Claude system.init), handleTurnStart must use the pending-send
// FIFO's TurnIndex rather than the LastTurnIndex store fallback.
//
// Background: the dispatcher computes the new turn index BEFORE the
// provider write (resolveFlushTurnPlacement) and stamps it into the
// pendingSend entry via RegisterPendingFlushSend. The wire echo then
// fires system.init for the new turn. Without consulting the pending-
// send entry, handleTurnStart falls back to LastTurnIndex — which
// returns the PREVIOUS turn's index because the deferred user_text for
// the new turn has not been persisted yet (handleUserText persists it
// only when the matching wire user_text echo arrives, which lands AFTER
// system.init). That mis-attribution causes setOpenTurn(t, prevTurn)
// to re-wipe the previous turn's stream-persist buffers and id-
// allocating counters, dropping trailing text from the previous turn
// and colliding the new turn's first text segment with the previous
// turn's text:N:0 row via UpsertItem's INSERT-OR-UPDATE.
func TestHandleTurnStart_PendingSendTurnIndex_PreservesQueueDispatchedTurn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Complete turn 0 so LastTurnIndex would return 0 if consulted.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     "t1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 complete: %v", err)
	}

	// Register a pending flush-send for turn 1. Mirrors what
	// dispatchFlushItem does before writing the new user message to
	// the provider.
	queuedItem := store.Item{
		ID:        "user:1:flush:0",
		ThreadID:  "t1",
		TurnIndex: 1,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "commit",
	}
	router.RegisterPendingFlushSendWithExpectation("t1", "queue:abc", queuedItem, 0, PendingSendExpectation{})

	// Provider echoes system.init (Claude wire shape: no turn index).
	// handleInit sees the pending-send and routes through
	// handleTurnStart.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventInit,
		ThreadID:  "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("system.init: %v", err)
	}

	// The new turn must open at the dispatcher-computed index (1),
	// NOT the LastTurnIndex fallback value (0).
	openTurn, ok := router.openTurnIndex("t1")
	if !ok {
		t.Fatalf("expected an open turn after handleInit→handleTurnStart")
	}
	if openTurn != 1 {
		t.Fatalf("openTurnIndex = %d, want 1 (pending-send TurnIndex must override LastTurnIndex fallback)", openTurn)
	}

	// The turns row should land at turn index 1.
	if _, found, err := st.GetTurn("t1:1"); err != nil || !found {
		t.Fatalf("expected turns row at t1:1 (queue-dispatched turn): found=%v err=%v", found, err)
	}
	// And the previous turn's row must remain settled at index 0 —
	// the counter wipe in setOpenTurn must not unsettle the prior turn.
	prev, found, err := st.GetTurn("t1:0")
	if err != nil || !found {
		t.Fatalf("previous turn row t1:0 missing: found=%v err=%v", found, err)
	}
	if prev.CompletedAt == nil {
		t.Errorf("previous turn t1:0 completed_at unset after queue dispatch — counter wipe must not reset prior turn")
	}

	// And the pending-send entry must still be poppable — handleTurnStart
	// only READS the TurnIndex; the FIFO pop is owned by handleUserText
	// when the matching wire user_text echo arrives.
	if !router.HasPendingSendForThread("t1") {
		t.Fatal("handleTurnStart must not consume the pending-send entry — pop is owned by handleUserText")
	}
}

// TestHandleTurnStart_PendingSendTurnIndex_NewTurnSegmentsDoNotCollideWithPrevious
// is the end-to-end regression: after a queue dispatch opens the new
// turn under the right index, the new turn's first text segment must
// allocate at text:1:0 (fresh counter under turn 1) — not text:0:1 (a
// collision under the previous turn's counter that would silently
// overwrite turn 0's content via UpsertItem).
//
// This is the user-visible shape of the bug: streaming text from the
// new turn overwrites the trailing assistant text from the previous
// turn, so the previous turn's closing message vanishes from the chat
// timeline.
func TestHandleTurnStart_PendingSendTurnIndex_NewTurnSegmentsDoNotCollideWithPrevious(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Turn 0: stream a trailing assistant text segment, then complete.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   "trailing assistant text for turn 0",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 trailing text: %v", err)
	}
	if err := router.settleStreamingScope("t1", ""); err != nil {
		t.Fatalf("close turn 0 trailing text: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     "t1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 complete: %v", err)
	}
	router.WaitForPendingSettles()

	// Queue dispatch registers a pending flush-send for turn 1, then
	// the provider echoes system.init.
	queuedItem := store.Item{
		ID:        "user:1:flush:0",
		ThreadID:  "t1",
		TurnIndex: 1,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "next prompt",
	}
	router.RegisterPendingFlushSendWithExpectation("t1", "queue:abc", queuedItem, 0, PendingSendExpectation{})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventInit,
		ThreadID:  "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("system.init for new turn: %v", err)
	}

	// Stream the new turn's first text. With the fix this lands at
	// text:1:0. Without the fix it would have landed at text:0:1 and
	// the InsertOrUpdate would silently shift turn 0's text content.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  "t1",
		Content:   "Starting on the next task.",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 1 first text: %v", err)
	}
	if err := router.settleStreamingScope("t1", ""); err != nil {
		t.Fatalf("close turn 1 first text: %v", err)
	}
	router.WaitForPendingSettles()

	// The previous turn's trailing assistant_text row must remain
	// intact at text:0:0 with the original content.
	prev, found, err := st.GetThreadItem("t1", "text:0:0")
	if err != nil || !found {
		t.Fatalf("expected text:0:0 to remain in store: found=%v err=%v", found, err)
	}
	if prev.Summary == "" || prev.Summary != "trailing assistant text for turn 0" {
		t.Errorf("text:0:0.Summary = %q, want the original trailing-text content (overwrite would silently change this)", prev.Summary)
	}
	if prev.TurnIndex != 0 {
		t.Errorf("text:0:0.TurnIndex = %d, want 0", prev.TurnIndex)
	}

	// The new turn's first text must land at text:1:0 under turn 1.
	next, found, err := st.GetThreadItem("t1", "text:1:0")
	if err != nil || !found {
		t.Fatalf("expected text:1:0 (new turn's first segment): found=%v err=%v", found, err)
	}
	if next.TurnIndex != 1 {
		t.Errorf("text:1:0.TurnIndex = %d, want 1", next.TurnIndex)
	}
	if next.Summary != "Starting on the next task." {
		t.Errorf("text:1:0.Summary = %q, want %q", next.Summary, "Starting on the next task.")
	}
}

// TestHandleTurnStart_PendingSendTurnIndex_CodexWireShape_PrefersPendingOverLastTurnIndex
// pins the symmetric Codex behavior: Codex's `turn/started` notification
// classifies as EventTurnStart without populating TurnIndex (see
// internal/provider/codex/protocol_turn.go), so a queue-dispatched Codex
// turn lands in handleTurnStart with evt.TurnIndex == 0 just like
// Claude's system.init. The pending-send FIFO peek must win over the
// LastTurnIndex fallback for both wire shapes.
func TestHandleTurnStart_PendingSendTurnIndex_CodexWireShape_PrefersPendingOverLastTurnIndex(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     "t1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 complete: %v", err)
	}

	queuedItem := store.Item{
		ID:        "user:1:flush:0",
		ThreadID:  "t1",
		TurnIndex: 1,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "queued prompt",
	}
	router.RegisterPendingFlushSendWithExpectation("t1", "queue:abc", queuedItem, 0, PendingSendExpectation{})

	// Codex turn/started → EventTurnStart with no TurnIndex on the wire.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("codex turn/started: %v", err)
	}

	openTurn, ok := router.openTurnIndex("t1")
	if !ok {
		t.Fatalf("expected an open turn after Codex-shaped EventTurnStart")
	}
	if openTurn != 1 {
		t.Fatalf("openTurnIndex = %d, want 1 (pending-send TurnIndex must override LastTurnIndex for Codex wire shape too)", openTurn)
	}
	if !router.HasPendingSendForThread("t1") {
		t.Fatal("handleTurnStart must not consume the pending-send entry on Codex EventTurnStart")
	}
}

// TestHandleTurnStart_PendingSendTurnIndex_MultiEntryFIFO_HeadWins
// pins that when multiple pending-flush sends pile up (the user typed
// two messages while the previous turn was streaming and both queued
// before any echo arrived), resolveTurnIndexOnStart reads the FIFO head
// — the oldest unmatched send — not the tail. The dispatcher writes
// queued sends in user-typed order, so the next wire echo always
// corresponds to the oldest unmatched entry.
func TestHandleTurnStart_PendingSendTurnIndex_MultiEntryFIFO_HeadWins(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     "t1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 complete: %v", err)
	}

	// Two queued sends piled up (turn 1 first, then turn 2). FIFO head
	// is turn 1 — the wire echo for the next system.init corresponds to
	// that older send.
	router.RegisterPendingFlushSendWithExpectation("t1", "queue:1", store.Item{
		ID: "user:1:flush:0", ThreadID: "t1", TurnIndex: 1,
		Kind: "user_text", Role: "user", Status: "completed", Summary: "first queued",
	}, 0, PendingSendExpectation{})

	router.RegisterPendingFlushSendWithExpectation("t1", "queue:2", store.Item{
		ID: "user:2:flush:0", ThreadID: "t1", TurnIndex: 2,
		Kind: "user_text", Role: "user", Status: "completed", Summary: "second queued",
	}, 0, PendingSendExpectation{})

	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventInit,
		ThreadID:  "t1",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("system.init: %v", err)
	}

	openTurn, ok := router.openTurnIndex("t1")
	if !ok {
		t.Fatalf("expected an open turn after handleInit→handleTurnStart")
	}
	if openTurn != 1 {
		t.Fatalf("openTurnIndex = %d, want 1 (FIFO head, the oldest queued send) — got the wrong queue entry", openTurn)
	}
}

// TestDeferredFlushEcho_SecondEchoSettlesPredecessorTurn pins the
// multi-echo settle (R2): when Claude drains TWO queued messages in the
// same wire round, the round's single `result` will settle only the last
// open turn — the first queued turn would stay open forever. The second
// echo must settle its predecessor: streaming rows complete, turns row
// closed at the successor's start with stop_reason end_turn, and no
// provider:turn_completed emission (the successor's turn_started replaces
// the round snapshot in place).
func TestDeferredFlushEcho_SecondEchoSettlesPredecessorTurn(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 start: %v", err)
	}

	// Turn 0 is still running. First queued message consumed mid-loop.
	router.RegisterPendingFlushSendWithExpectation("t1", "queue:q1", store.Item{
		ID: "user:1:flush:1", ThreadID: "t1", TurnIndex: 1,
		Kind: "user_text", Role: "user", Status: "completed", Summary: "first queued",
	}, time.Now().UnixMilli(), PendingSendExpectation{ProviderItemID: "ao-uuid-1"})

	emissions.reset()
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: "t1", Content: "first queued",
		Meta:      json.RawMessage(`{"provider_item_id":"ao-uuid-1"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("first echo: %v", err)
	}
	// Its response starts streaming and is still mid-flight when the CLI
	// drains the next queued message — no settle, no wire result.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1",
		Content: "response to first", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("first response text: %v", err)
	}

	// Second queued message consumed in the same round.
	router.RegisterPendingFlushSendWithExpectation("t1", "queue:q2", store.Item{
		ID: "user:2:flush:1", ThreadID: "t1", TurnIndex: 2,
		Kind: "user_text", Role: "user", Status: "completed", Summary: "second queued",
	}, time.Now().UnixMilli(), PendingSendExpectation{ProviderItemID: "ao-uuid-2"})

	echo2At := time.Now().Add(3 * time.Second)
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: "t1", Content: "second queued",
		Meta:      json.RawMessage(`{"provider_item_id":"ao-uuid-2"}`),
		Timestamp: echo2At,
	}); err != nil {
		t.Fatalf("second echo: %v", err)
	}
	router.WaitForPendingSettles()

	if openTurn, ok := router.openTurnIndex("t1"); !ok || openTurn != 2 {
		t.Fatalf("openTurnIndex = %d ok=%v, want 2", openTurn, ok)
	}
	// Predecessor turn 1: settled at the second echo's timestamp.
	turn1, found, err := st.GetTurn("t1:1")
	if err != nil || !found {
		t.Fatalf("turns row t1:1: found=%v err=%v", found, err)
	}
	if turn1.CompletedAt == nil || *turn1.CompletedAt != echo2At.UnixMilli() {
		t.Fatalf("turn 1 completed_at = %v, want second echo time %d", turn1.CompletedAt, echo2At.UnixMilli())
	}
	if turn1.StopReason != "end_turn" {
		t.Errorf("turn 1 stop_reason = %q, want end_turn", turn1.StopReason)
	}
	// Its still-streaming response row settled as completed.
	text1, found, err := st.GetThreadItem("t1", "text:1:0")
	if err != nil || !found {
		t.Fatalf("text:1:0: found=%v err=%v", found, err)
	}
	if text1.Status != "completed" {
		t.Errorf("text:1:0 status = %q, want completed (predecessor settle must close streaming rows)", text1.Status)
	}
	// Turn 2 is open, not settled.
	turn2, found, err := st.GetTurn("t1:2")
	if err != nil || !found {
		t.Fatalf("turns row t1:2: found=%v err=%v", found, err)
	}
	if turn2.CompletedAt != nil {
		t.Errorf("turn 2 completed_at set at open")
	}

	// Emissions: turn_started for 1 and 2, and NO turn_completed — the
	// successor's turn_started replaces the round snapshot.
	var startedIdx []int
	completedCount := 0
	for _, e := range emissions.snapshot() {
		switch e.eventName {
		case "provider:turn_started":
			startedIdx = append(startedIdx, e.data.(TurnStartedEvent).TurnIndex)
		case "provider:turn_completed":
			completedCount++
		}
	}
	if len(startedIdx) != 2 || startedIdx[0] != 1 || startedIdx[1] != 2 {
		t.Fatalf("turn_started indexes = %v, want [1 2]", startedIdx)
	}
	if completedCount != 0 {
		t.Fatalf("turn_completed emissions = %d, want 0 (predecessor settle is store-only)", completedCount)
	}

	// The round's single wire result settles the LAST turn.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: normalTurnCompleteMeta(), Timestamp: echo2At.Add(4 * time.Second),
	}); err != nil {
		t.Fatalf("round result: %v", err)
	}
	settled, found, err := st.GetTurn("t1:2")
	if err != nil || !found || settled.CompletedAt == nil {
		t.Fatalf("turns row t1:2 not settled by the round result: found=%v completedAt=%v err=%v", found, settled.CompletedAt, err)
	}
}

// TestOpenQueuedEchoTurn_RefusesBackwardAndSettledIndexes pins the
// guards that make openQueuedEchoTurn safe from the attach-path caller,
// which fires on every WasDeferred echo including session-resume
// replays: an index behind the open turn and an already-settled index
// are both refused — reopening either would reset id-allocating
// counters under rows already persisted there (the invariant-27 hazard).
func TestOpenQueuedEchoTurn_RefusesBackwardAndSettledIndexes(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	now := time.Now().UnixMilli()

	router.openQueuedEchoTurn("t1", 1, now, -1)
	if openTurn, ok := router.openTurnIndex("t1"); !ok || openTurn != 1 {
		t.Fatalf("openTurnIndex = %d ok=%v, want 1", openTurn, ok)
	}

	// Same index again: idempotent no-op.
	emissions.reset()
	router.openQueuedEchoTurn("t1", 1, now+10, -1)
	if got := len(emissions.snapshot()); got != 0 {
		t.Fatalf("re-open of the open index emitted %d events, want 0", got)
	}

	// Backward: refused, open turn unchanged, no turns row minted.
	router.openQueuedEchoTurn("t1", 0, now+20, -1)
	if openTurn, ok := router.openTurnIndex("t1"); !ok || openTurn != 1 {
		t.Fatalf("openTurnIndex after backward call = %d ok=%v, want still 1", openTurn, ok)
	}
	if _, found, err := st.GetTurn("t1:0"); err != nil {
		t.Fatalf("get t1:0: %v", err)
	} else if found {
		t.Fatal("backward call minted a turns row for the refused index")
	}
	if got := len(emissions.snapshot()); got != 0 {
		t.Fatalf("refused calls emitted %d events, want 0", got)
	}

	// Settle turn 1 through the wire result, then replay the echo open:
	// the settled marker must refuse the reopen.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: normalTurnCompleteMeta(), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 1 complete: %v", err)
	}
	settled, found, err := st.GetTurn("t1:1")
	if err != nil || !found || settled.CompletedAt == nil {
		t.Fatalf("turn 1 not settled: found=%v err=%v", found, err)
	}
	emissions.reset()
	router.openQueuedEchoTurn("t1", 1, now+40, -1)
	if _, ok := router.openTurnIndex("t1"); ok {
		t.Fatal("replayed echo reopened a settled turn")
	}
	after, _, err := st.GetTurn("t1:1")
	if err != nil {
		t.Fatalf("get t1:1 after replay: %v", err)
	}
	if after.CompletedAt == nil || *after.CompletedAt != *settled.CompletedAt {
		t.Fatalf("replay moved the settled turn's completed_at: %v -> %v", settled.CompletedAt, after.CompletedAt)
	}
	if got := len(emissions.snapshot()); got != 0 {
		t.Fatalf("settled-index replay emitted %d events, want 0", got)
	}
}

// TestEagerPersistedDeferredFlushEcho_OpensLogicalTurn pins the C2 fix:
// a deferred flush row that the INTERRUPT eagerly persisted (WasDeferred)
// still owns a fresh turn index, and when its echo arrives on the
// still-live old round — the interrupt raced the CLI's mid-loop queue
// drain, so no system.init opened the new turn — the attach path must
// open the logical turn exactly like the still-deferred persist path
// does, settling the interrupted predecessor.
func TestEagerPersistedDeferredFlushEcho_OpensLogicalTurn(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 start: %v", err)
	}

	router.RegisterPendingFlushSendWithExpectation("t1", "queue:q1", store.Item{
		ID: "user:1:flush:1", ThreadID: "t1", TurnIndex: 1,
		Kind: "user_text", Role: "user", Status: "completed", Summary: "queued",
	}, time.Now().UnixMilli(), PendingSendExpectation{ProviderItemID: "ao-uuid-1"})

	// Interrupt: the deferred row persists eagerly at the turn tail.
	persisted := eagerPersistForTest(router, "t1", router.OpenTurnIndex("t1"))
	if len(persisted) != 1 || persisted[0].UserItemID != "user:1:flush:1" {
		t.Fatalf("eager persist = %+v, want the queued row", persisted)
	}

	// The echo lands while turn 0's round is still live — no init between.
	emissions.reset()
	echoAt := time.Now().Add(2 * time.Second)
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: "t1", Content: "queued",
		Meta:      json.RawMessage(`{"provider_item_id":"ao-uuid-1"}`),
		Timestamp: echoAt,
	}); err != nil {
		t.Fatalf("echo: %v", err)
	}
	router.WaitForPendingSettles()

	if openTurn, ok := router.openTurnIndex("t1"); !ok || openTurn != 1 {
		t.Fatalf("openTurnIndex = %d ok=%v, want 1 (WasDeferred echo must open the turn)", openTurn, ok)
	}
	if _, found, err := st.GetTurn("t1:1"); err != nil || !found {
		t.Fatalf("turns row t1:1 missing: found=%v err=%v", found, err)
	}
	// The interrupted predecessor settled at the echo — and records the
	// interrupt, not a natural completion: this settle claims the turn,
	// so the wire's truncated result can never correct the reason later.
	turn0, found, err := st.GetTurn("t1:0")
	if err != nil || !found {
		t.Fatalf("turns row t1:0: found=%v err=%v", found, err)
	}
	if turn0.CompletedAt == nil || *turn0.CompletedAt != echoAt.UnixMilli() {
		t.Fatalf("turn 0 completed_at = %v, want echo time %d", turn0.CompletedAt, echoAt.UnixMilli())
	}
	if turn0.StopReason != "interrupted" {
		t.Errorf("turn 0 stop_reason = %q, want interrupted (WasDeferred row proves a user interrupt)", turn0.StopReason)
	}
	var started []TurnStartedEvent
	for _, e := range emissions.snapshot() {
		if e.eventName == "provider:turn_started" {
			started = append(started, e.data.(TurnStartedEvent))
		}
	}
	if len(started) != 1 || started[0].TurnIndex != 1 {
		t.Fatalf("turn_started emissions = %+v, want exactly one at index 1", started)
	}
	// The row is stamped (attach path ran), and was NOT re-bumped — it
	// keeps the index the interrupt persist gave it.
	row, found, err := st.GetThreadItem("t1", "user:1:flush:1")
	if err != nil || !found {
		t.Fatalf("flush row: found=%v err=%v", found, err)
	}
	if row.TurnIndex != 1 {
		t.Errorf("flush row turn_index = %d, want 1", row.TurnIndex)
	}
}

// A turn start the provider attributed to another producer
// (`origin: external-queue` — `codex queue --thread` wrote into the same FIFO
// the app-server drains) must not read its index off the pending-send head.
// The head names a message this app dispatched and is still waiting on; the
// foreign turn taking its index is a SQUAT, and the pending send's own echo
// then collides with it on UNIQUE(thread_id, turn_index) when it opens the
// turn it was promised (2026-08-24). The foreign turn gets a turn of its own,
// after everything known.
func TestHandleTurnStart_ExternalOrigin_DoesNotStealThePendingSendTurnIndex(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Turn 0 ran and settled, so LastTurnIndex reads 0.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     "t1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 complete: %v", err)
	}

	// AO dispatched a queued message for turn 1; its row is deferred and
	// therefore invisible to LastTurnIndex.
	router.RegisterPendingFlushSendWithExpectation("t1", "queue:ours", store.Item{
		ID:        "user:1:flush:1",
		ThreadID:  "t1",
		TurnIndex: 1,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "ours",
	}, 10, PendingSendExpectation{ByClientID: true})

	// The foreign turn dispatches first. Codex's turn/started carries no
	// turn index, only the provider turn id and the origin stamp.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  "t1",
		TurnID:    "codex-turn-foreign",
		Meta:      json.RawMessage(`{"origin":"external-queue"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("foreign turn/started: %v", err)
	}

	openTurn, ok := router.openTurnIndex("t1")
	if !ok {
		t.Fatal("expected an open turn after the foreign turn start")
	}
	if openTurn == 1 {
		t.Fatal("the foreign turn squatted on the pending send's turn index 1")
	}
	if openTurn != 2 {
		t.Fatalf("openTurnIndex = %d, want 2 (past the last known turn AND past every pending send)", openTurn)
	}
	if _, found, err := st.GetTurnByThreadIndex("t1", 2); err != nil || !found {
		t.Fatalf("foreign turns row missing at index 2: found=%v err=%v", found, err)
	}
	if _, found, err := st.GetTurnByThreadIndex("t1", 1); err == nil && found {
		t.Fatal("turn index 1 was taken — it belongs to the pending send")
	}

	// The pending send is untouched and still names turn 1.
	head, ok := router.PeekPendingSendHeadForTest("t1")
	if !ok || head.AOItemID != "user:1:flush:1" || head.TurnIndex != 1 {
		t.Fatalf("pending head = %+v ok=%v, want user:1:flush:1 at turn 1", head, ok)
	}
}

// Same rule with no pending send at all: an external turn on an otherwise
// settled thread still opens a NEW turn rather than reusing the last one.
func TestHandleTurnStart_ExternalOrigin_AllocatesAfterTheLastKnownTurn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 start: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: normalTurnCompleteMeta(), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn 0 complete: %v", err)
	}

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnID: "codex-turn-foreign",
		Meta: json.RawMessage(`{"origin":"external-queue"}`), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("foreign turn/started: %v", err)
	}

	if openTurn, ok := router.openTurnIndex("t1"); !ok || openTurn != 1 {
		t.Fatalf("openTurnIndex = %d ok=%v, want 1 (LastTurnIndex+1)", openTurn, ok)
	}
	if _, found, err := st.GetTurnByThreadIndex("t1", 1); err != nil || !found {
		t.Fatalf("foreign turns row missing at index 1: found=%v err=%v", found, err)
	}
}

// An unknown origin is not an attribution: the pending-send peek stays the
// answer for every turn start this app could have provoked.
func TestHandleTurnStart_UnknownOrigin_KeepsThePendingSendTurnIndex(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	router.RegisterPendingSendWithExpectation("t1", "user:4", 4, PendingSendExpectation{})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnID: "codex-turn-1",
		Meta: json.RawMessage(`{"origin":"something-new"}`), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn/started: %v", err)
	}
	if openTurn, ok := router.openTurnIndex("t1"); !ok || openTurn != 4 {
		t.Fatalf("openTurnIndex = %d ok=%v, want 4 (pending-send head)", openTurn, ok)
	}
}

// upsertTurnRow used to log and swallow the UNIQUE(thread_id, turn_index)
// failure, which left the incoming turn with no row of its own: its settle
// then folded onto the SQUATTER's row via persistedTurnID. Two provably
// distinct provider turns on one index relocate the incoming one instead.
func TestUpsertTurnRow_DistinctProviderTurnsCollide_RelocatesTheIncomingTurn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	now := time.Now().UnixMilli()
	if err := st.InsertTurn(store.Turn{
		TurnID: "t1:prov-a", ThreadID: "t1", TurnIndex: 1,
		StartedAt: now, ProviderTurnID: "prov-a",
	}); err != nil {
		t.Fatalf("seed standing turn: %v", err)
	}

	placed := router.upsertTurnRow(store.Turn{
		TurnID: "t1:prov-b", ThreadID: "t1", TurnIndex: 1,
		StartedAt: now + 1, ProviderTurnID: "prov-b",
	})
	if placed == 1 {
		t.Fatal("the incoming turn was left colliding on index 1")
	}
	if placed != 2 {
		t.Fatalf("placed index = %d, want 2 (the next free index)", placed)
	}

	relocated, found, err := st.GetTurnByThreadIndex("t1", 2)
	if err != nil || !found {
		t.Fatalf("relocated row missing: found=%v err=%v", found, err)
	}
	if relocated.ProviderTurnID != "prov-b" {
		t.Fatalf("relocated provider turn = %q, want prov-b", relocated.ProviderTurnID)
	}
	standing, found, err := st.GetTurnByThreadIndex("t1", 1)
	if err != nil || !found {
		t.Fatalf("standing row lost: found=%v err=%v", found, err)
	}
	if standing.TurnID != "t1:prov-a" || standing.StartedAt != now {
		t.Fatalf("standing row was disturbed: %+v", standing)
	}
}

// The reverse-order guard. openQueuedEchoTurn mints `<thread>:<index>`; a
// Codex wire start for the SAME logical turn mints
// `<thread>:<providerTurnID>`. One index, two id shapes, one turn — adopt the
// standing row rather than inserting a second one (which is what raised the
// UNIQUE violation) and rather than relocating (which would split one turn's
// rows across two indexes).
func TestUpsertTurnRow_EchoOpenedIndex_AdoptedByTheProviderTurnStart(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	startedAt := time.Now().UnixMilli()
	router.openQueuedEchoTurn("t1", 1, startedAt, -1)
	if _, found, err := st.GetTurn("t1:1"); err != nil || !found {
		t.Fatalf("echo-opened turns row missing: found=%v err=%v", found, err)
	}

	// The wire start arrives afterwards, carrying the provider's own id.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1,
		TurnID: "codex-turn-1", Timestamp: time.UnixMilli(startedAt + 5),
	}); err != nil {
		t.Fatalf("wire turn/started: %v", err)
	}

	if openTurn, ok := router.openTurnIndex("t1"); !ok || openTurn != 1 {
		t.Fatalf("openTurnIndex = %d ok=%v, want 1 (the adopted turn)", openTurn, ok)
	}
	turns, err := st.ListRecentTurns("t1", 10)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("turns rows = %d (%+v), want exactly 1 — the wire start must adopt, not re-insert", len(turns), turns)
	}
	if turns[0].TurnID != "t1:1" || turns[0].TurnIndex != 1 {
		t.Fatalf("surviving row = %+v, want the echo-opened t1:1 at index 1", turns[0])
	}
	if turns[0].StartedAt != startedAt {
		t.Fatalf("started_at = %d, want the echo's %d (the existing row is authoritative)", turns[0].StartedAt, startedAt)
	}
	// The adoption backfills the wire's provider turn id onto the
	// echo-opened row: it is the collision discriminator (blank always
	// reads as "adopt") and the Codex fork/revert anchor.
	if turns[0].ProviderTurnID != "codex-turn-1" {
		t.Fatalf("provider_turn_id = %q, want the adopted wire start's codex-turn-1", turns[0].ProviderTurnID)
	}
}

// The forward order is still the openQueuedEchoTurn guard's job, and the row
// the wire start wrote is what survives.
func TestUpsertTurnRow_ProviderTurnStartThenEcho_KeepsOneRow(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	startedAt := time.Now().UnixMilli()
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1,
		TurnID: "codex-turn-1", Timestamp: time.UnixMilli(startedAt),
	}); err != nil {
		t.Fatalf("wire turn/started: %v", err)
	}
	router.openQueuedEchoTurn("t1", 1, startedAt+5, -1)

	turns, err := st.ListRecentTurns("t1", 10)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("turns rows = %d (%+v), want exactly 1", len(turns), turns)
	}
	if turns[0].TurnID != "t1:codex-turn-1" {
		t.Fatalf("surviving row = %+v, want the wire start's t1:codex-turn-1", turns[0])
	}
}

// isTurnIndexCollisionError matches modernc.org/sqlite's message by
// substring — the driver exposes no typed error — so the spelling is pinned
// against the real refusal rather than a remembered one. A driver that
// reworded it would otherwise turn the re-resolution path back into the
// log-and-swallow it replaced, silently.
func TestIsTurnIndexCollisionError_MatchesTheDriversRefusal(t *testing.T) {
	_, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	now := time.Now().UnixMilli()
	if err := st.InsertTurn(store.Turn{TurnID: "a", ThreadID: "t1", TurnIndex: 1, StartedAt: now}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	err := st.InsertTurn(store.Turn{TurnID: "b", ThreadID: "t1", TurnIndex: 1, StartedAt: now})
	if err == nil {
		t.Fatal("a second turns row at one (thread, turn_index) was accepted")
	}
	if !isTurnIndexCollisionError(err) {
		t.Fatalf("isTurnIndexCollisionError did not recognise %v", err)
	}
	if isTurnIndexCollisionError(nil) {
		t.Fatal("nil read as a collision")
	}
}
