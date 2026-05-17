package triage

import (
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
	router.RegisterPendingFlushSend("t1", "queue:abc", queuedItem)

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
	router.RegisterPendingFlushSend("t1", "queue:abc", queuedItem)
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
	router.RegisterPendingFlushSend("t1", "queue:abc", queuedItem)

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
	router.RegisterPendingFlushSend("t1", "queue:1", store.Item{
		ID: "user:1:flush:0", ThreadID: "t1", TurnIndex: 1,
		Kind: "user_text", Role: "user", Status: "completed", Summary: "first queued",
	})
	router.RegisterPendingFlushSend("t1", "queue:2", store.Item{
		ID: "user:2:flush:0", ThreadID: "t1", TurnIndex: 2,
		Kind: "user_text", Role: "user", Status: "completed", Summary: "second queued",
	})

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
