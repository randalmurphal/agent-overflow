package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// A Stop issued after a send reached the provider but before its turn
// start reached the router cuts the turn that send opens.

func initEvent(threadID string) provider.ProviderEvent {
	return provider.ProviderEvent{Kind: provider.EventInit, ThreadID: threadID, Timestamp: time.Now()}
}

// stoppedRows returns the thread's "Stopped by user" rows.
func stoppedRows(t *testing.T, st *store.Store, threadID string) []store.Item {
	t.Helper()
	items, err := st.ListItems(threadID)
	if err != nil {
		t.Fatal(err)
	}
	var rows []store.Item
	for _, item := range items {
		if item.Kind == ItemKindError && item.Summary == StoppedByUser {
			rows = append(rows, item)
		}
	}
	return rows
}

// requireOneStoppedRow asserts the thread has exactly one stop row, on
// turnIndex, and returns its id.
func requireOneStoppedRow(t *testing.T, st *store.Store, threadID string, turnIndex int) string {
	t.Helper()
	rows := stoppedRows(t, st, threadID)
	if len(rows) != 1 {
		t.Fatalf("stop rows = %d, want 1", len(rows))
	}
	if rows[0].TurnIndex != turnIndex {
		t.Fatalf("stop row on turn %d, want %d", rows[0].TurnIndex, turnIndex)
	}
	return rows[0].ID
}

// The sample names the waiting send's turn and the stop row is written
// on it at once. The row survives the turn opening and a later error in
// it, and the turn after it is not marked.
func TestMarkInterrupt_WaitingSendMarkedBeforeItsTurnOpens(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	router.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{})

	sample := router.SampleInterrupt("t1")
	if sample.Turn != -1 || sample.CutTurn() != 1 {
		t.Fatalf("sample = open %d cut %d, want open -1 cut 1", sample.Turn, sample.CutTurn())
	}
	tok := router.MarkFlushSendsInterrupted("t1", sample.Turn)
	errID, err := router.MarkInterrupt("t1", sample, tok, StoppedByUser)
	if err != nil {
		t.Fatalf("MarkInterrupt: %v", err)
	}
	requireStoppedByUserRow(t, st, "t1", errID, 1)

	if err := router.Handle(initEvent("t1")); err != nil {
		t.Fatalf("init: %v", err)
	}
	if got := router.OpenTurnIndex("t1"); got != 1 {
		t.Fatalf("open turn = %d, want 1", got)
	}
	requireOneStoppedRow(t, st, "t1", 1)
	requireMarkerSurvivesLaterError(t, router, st, "t1", errID, 1)

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "interrupted"},
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}
	router.RegisterPendingSendWithExpectation("t1", "user:2", 2, PendingSendExpectation{})
	if err := router.Handle(initEvent("t1")); err != nil {
		t.Fatalf("next init: %v", err)
	}
	requireOneStoppedRow(t, st, "t1", 1)
}

// The waiting send's turn opened during the ack wait: the row lands on
// that turn.
func TestMarkInterrupt_WaitingSendOpenedDuringAck(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	router.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{})

	sample := router.SampleInterrupt("t1")
	tok := router.MarkFlushSendsInterrupted("t1", sample.Turn)
	if err := router.Handle(initEvent("t1")); err != nil {
		t.Fatalf("init: %v", err)
	}
	errID, err := router.MarkInterrupt("t1", sample, tok, StoppedByUser)
	if err != nil {
		t.Fatalf("MarkInterrupt: %v", err)
	}
	requireStoppedByUserRow(t, st, "t1", errID, 1)
	requireOneStoppedRow(t, st, "t1", 1)
	requireMarkerSurvivesLaterError(t, router, st, "t1", errID, 1)
}

// The waiting send's echo opens its turn before any init does: the stop
// row is the turn's own, and the init that follows adds nothing.
func TestMarkInterrupt_WaitingSendOpenedByItsEcho(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	now := time.Now().UnixMilli()
	if _, err := st.AppendItem(store.Item{
		ID: "user:1", ThreadID: "t1", TurnIndex: 1, Kind: "user_text", Role: "user",
		Status: "completed", Summary: "just sent", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("append user row: %v", err)
	}
	router.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{ProviderItemID: "echo-1"})

	sample := router.SampleInterrupt("t1")
	tok := router.MarkFlushSendsInterrupted("t1", sample.Turn)
	errID, err := router.MarkInterrupt("t1", sample, tok, StoppedByUser)
	if err != nil {
		t.Fatalf("MarkInterrupt: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: "t1", Content: "just sent",
		Meta: json.RawMessage(`{"provider_item_id":"echo-1"}`), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("echo: %v", err)
	}
	if got := router.OpenTurnIndex("t1"); got != 1 {
		t.Fatalf("open turn after the echo = %d, want 1", got)
	}
	if err := router.Handle(initEvent("t1")); err != nil {
		t.Fatalf("init: %v", err)
	}
	requireOneStoppedRow(t, st, "t1", 1)
	requireMarkerSurvivesLaterError(t, router, st, "t1", errID, 1)
}

// A stop row written while the waiting send's turn start is in progress
// survives the start.
func TestMarkInterrupt_WaitingSendStartInProgress(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	router.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{})

	sample := router.SampleInterrupt("t1")
	tok := router.MarkFlushSendsInterrupted("t1", sample.Turn)
	held := holdTurnStart(t, router, initEvent("t1"), false)
	errID, err := router.MarkInterrupt("t1", sample, tok, StoppedByUser)
	if err != nil {
		t.Fatalf("MarkInterrupt: %v", err)
	}
	held.release()
	select {
	case startErr := <-held.handled:
		if startErr != nil {
			t.Fatalf("init: %v", startErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("released turn start never returned")
	}
	requireStoppedByUserRow(t, st, "t1", errID, 1)
	requireOneStoppedRow(t, st, "t1", 1)
	requireMarkerSurvivesLaterError(t, router, st, "t1", errID, 1)
}

// A steer the provider never confirmed is no waiting send: it joined a
// turn that has since ended and opens none. The direct send registered
// after it is the one an interrupt cuts.
func TestSampleInterrupt_UnconfirmedSteerIsNotAWaitingSend(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 1)
	router.RegisterPendingSteerSendWithExpectation("t1", "user:1:steer:1", 1, PendingSendExpectation{ByClientID: true})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "interrupted"},
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}

	if sample := router.SampleInterrupt("t1"); sample.Turn != -1 || sample.CutTurn() != -1 {
		t.Fatalf("sample with only the steer = open %d cut %d, want nothing to cut", sample.Turn, sample.CutTurn())
	}
	router.RegisterPendingSendWithExpectation("t1", "user:2", 2, PendingSendExpectation{ByClientID: true})
	if sample := router.SampleInterrupt("t1"); sample.CutTurn() != 2 {
		t.Fatalf("cut turn = %d, want the direct send's turn 2", sample.CutTurn())
	}
}
