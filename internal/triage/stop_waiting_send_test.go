package triage

import (
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

// The sample names the waiting send's turn, and its stop row is written
// once the turn opens. The turn after it is not marked.
func TestMarkInterrupt_WaitingSendMarkedWhenItsTurnOpens(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	router.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{})

	sample := router.SampleInterrupt("t1")
	if sample.Turn != -1 || sample.CutTurn() != 1 {
		t.Fatalf("sample = open %d cut %d, want open -1 cut 1", sample.Turn, sample.CutTurn())
	}
	tok := router.MarkFlushSendsInterrupted("t1", sample.Turn)
	errID, err := router.MarkInterrupt("t1", sample, tok, StoppedByUser)
	if err != nil || errID != "" {
		t.Fatalf("MarkInterrupt = %q, %v; want the row deferred to the turn start", errID, err)
	}
	if rows := stoppedRows(t, st, "t1"); len(rows) != 0 {
		t.Fatalf("stop row written before its turn opened: %+v", rows)
	}

	if err := router.Handle(initEvent("t1")); err != nil {
		t.Fatalf("init: %v", err)
	}
	if got := router.OpenTurnIndex("t1"); got != 1 {
		t.Fatalf("open turn = %d, want 1", got)
	}
	errID = requireOneStoppedRow(t, st, "t1", 1)
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

// The waiting send's turn opened during the ack wait: the row is
// written by MarkInterrupt itself, on that turn.
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

// The waiting send's turn start is in progress when MarkInterrupt runs:
// it waits for the open turn rather than writing a row whose id the
// start's setOpenTurn would reuse.
func TestMarkInterrupt_WaitingSendStartInProgress(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	router.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{})

	sample := router.SampleInterrupt("t1")
	tok := router.MarkFlushSendsInterrupted("t1", sample.Turn)
	held := holdTurnStart(t, router, initEvent("t1"), false)
	res, startErr := runThroughHeldStart(t, router, "t1", held, func() interruptResult {
		errID, err := router.MarkInterrupt("t1", sample, tok, StoppedByUser)
		return interruptResult{errID: errID, err: err}
	})
	if startErr != nil {
		t.Fatalf("init: %v", startErr)
	}
	if res.err != nil {
		t.Fatalf("MarkInterrupt: %v", res.err)
	}
	requireStoppedByUserRow(t, st, "t1", res.errID, 1)
	requireOneStoppedRow(t, st, "t1", 1)
	requireMarkerSurvivesLaterError(t, router, st, "t1", res.errID, 1)
}
