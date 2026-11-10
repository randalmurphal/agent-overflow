package triage

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// queueDuringTurnZero registers a message queued while turn 0 runs: its row
// sits in turn 0 and it opens turn 1 if the provider picks it up afterwards.
func queueDuringTurnZero(t *testing.T, router *Router) store.Item {
	t.Helper()
	now := time.Now().UnixMilli()
	row := store.Item{ID: "user:0:flush:1", ThreadID: "t1", TurnIndex: 0, Kind: "user_text", Role: "user", Status: "completed", Summary: "queued", CreatedAt: now, UpdatedAt: now}
	if err := router.PersistAndRegisterPendingQuietFlushSendWithExpectation("t1", "queue-1", row, 1, now, PendingSendExpectation{ProviderItemID: "echo-1"}); err != nil {
		t.Fatalf("register queued send: %v", err)
	}
	return row
}

func queuedSendEcho() provider.ProviderEvent {
	return provider.ProviderEvent{Kind: provider.EventUserText, ThreadID: "t1", Content: "queued", Meta: json.RawMessage(`{"provider_item_id":"echo-1"}`), Timestamp: time.Now()}
}

func assertQueuedSendTurn(t *testing.T, router *Router, itemID, when string, wantTurn int, wantOpened, wantWaiting bool) {
	t.Helper()
	turn, opened, waiting := router.QueuedSendTurn("t1", itemID)
	if turn != wantTurn || opened != wantOpened || waiting != wantWaiting {
		t.Fatalf("%s: QueuedSendTurn = turn %d opened %v waiting %v, want turn %d opened %v waiting %v",
			when, turn, opened, waiting, wantTurn, wantOpened, wantWaiting)
	}
}

// A message the provider picks up after the turn it was queued in waits for
// turn 1, then runs there. The answer outlives the pending entry, which the
// echo removes, and ends with the session.
func TestQueuedSendPickedUpAfterItsTurnReportsTheTurnItOpened(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0, Timestamp: time.Now()}); err != nil {
		t.Fatalf("turn 0 start: %v", err)
	}
	row := queueDuringTurnZero(t, router)
	assertQueuedSendTurn(t, router, row.ID, "while turn 0 runs", 1, false, true)

	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: normalTurnCompleteMeta(), Timestamp: time.Now()}); err != nil {
		t.Fatalf("turn 0 complete: %v", err)
	}
	assertQueuedSendTurn(t, router, row.ID, "after turn 0 ended without it", 1, false, true)

	if err := router.Handle(openerInit(t, provider.TurnOpenerCommand)); err != nil {
		t.Fatalf("pickup init: %v", err)
	}
	assertQueuedSendTurn(t, router, row.ID, "after the pickup init", 1, true, false)

	if err := router.Handle(queuedSendEcho()); err != nil {
		t.Fatalf("echo: %v", err)
	}
	if _, pending := router.turnOpeningPendingSend("t1"); pending {
		t.Fatal("the echo left the send waiting for a turn")
	}
	assertQueuedSendTurn(t, router, row.ID, "after the echo", 1, true, false)

	router.MarkThreadActive("t1")
	assertQueuedSendTurn(t, router, row.ID, "after a new session", 0, false, false)
}

// A message the provider drains into the running turn opens no turn: its
// row's turn answers it.
func TestQueuedSendDrainedIntoItsTurnReportsNoOtherTurn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0, Timestamp: time.Now()}); err != nil {
		t.Fatalf("turn 0 start: %v", err)
	}
	row := queueDuringTurnZero(t, router)

	if err := router.Handle(queuedSendEcho()); err != nil {
		t.Fatalf("echo: %v", err)
	}
	if open, ok := router.openTurnIndex("t1"); !ok || open != 0 {
		t.Fatalf("open turn = %d/%v, want the drain to stay in turn 0", open, ok)
	}
	assertQueuedSendTurn(t, router, row.ID, "after the drain", 0, false, false)
}

// A send that opens the turn its own row sits in is read from the row.
func TestSendThatOpensItsOwnRowsTurnIsNotRecorded(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	settleTurnZero(t, router)
	persistSendRow(t, router, "user:1", 1)
	if err := router.Handle(openerInit(t, provider.TurnOpenerCommand)); err != nil {
		t.Fatalf("init: %v", err)
	}
	assertQueuedSendTurn(t, router, "user:1", "after its init", 0, false, false)
}

func TestQueuedSendTurnsKeepOnlyTheNewestTurns(t *testing.T) {
	st := &threadState{}
	for turn := 1; turn <= queuedSendTurnHistory+3; turn++ {
		st.recordQueuedSendTurn(fmt.Sprintf("row-%d", turn), turn)
	}
	if len(st.queuedSendTurns) != queuedSendTurnHistory {
		t.Fatalf("kept %d entries, want %d", len(st.queuedSendTurns), queuedSendTurnHistory)
	}
	for turn := 1; turn <= 3; turn++ {
		if _, kept := st.queuedSendTurns[fmt.Sprintf("row-%d", turn)]; kept {
			t.Fatalf("turn %d survived, want the lowest turns dropped first", turn)
		}
	}
	if got := st.queuedSendTurns[fmt.Sprintf("row-%d", queuedSendTurnHistory+3)]; got != queuedSendTurnHistory+3 {
		t.Fatalf("newest entry = %d, want %d", got, queuedSendTurnHistory+3)
	}
}
