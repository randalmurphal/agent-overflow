package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// A Stop issued after a send reached the provider but before its turn
// start reached the router is held until that turn settles: the turn gets
// the stop row when it ends interrupted, and nothing when it ends normally
// or never runs.

func initEvent(threadID string) provider.ProviderEvent {
	return provider.ProviderEvent{Kind: provider.EventInit, ThreadID: threadID, Timestamp: time.Now()}
}

func turnCompleteEvent(threadID, stopReason string) provider.ProviderEvent {
	return provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: threadID,
		TurnComplete: &provider.WireTurnCompleteMeta{StopReason: stopReason},
		Timestamp:    time.Now(),
	}
}

func handleStopEvent(t *testing.T, router *Router, evt provider.ProviderEvent) {
	t.Helper()
	if err := router.Handle(evt); err != nil {
		t.Fatalf("%s: %v", evt.Kind, err)
	}
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

func requireNoStoppedRow(t *testing.T, st *store.Store, threadID string) {
	t.Helper()
	if rows := stoppedRows(t, st, threadID); len(rows) != 0 {
		t.Fatalf("stop rows = %+v, want none", rows)
	}
}

// stopWaitingSend runs the interrupt flow's triage half against the
// thread's waiting send and requires that it writes nothing yet.
func stopWaitingSend(t *testing.T, router *Router, st *store.Store, threadID string, cutTurn int) {
	t.Helper()
	sample := router.SampleInterrupt(threadID)
	if sample.Turn != -1 || sample.CutTurn() != cutTurn {
		t.Fatalf("sample = open %d cut %d, want open -1 cut %d", sample.Turn, sample.CutTurn(), cutTurn)
	}
	tok := router.MarkFlushSendsInterrupted(threadID, sample.Turn)
	errID, err := router.MarkInterrupt(threadID, sample, tok, StoppedByUser)
	if err != nil {
		t.Fatalf("MarkInterrupt: %v", err)
	}
	if errID != "" {
		t.Fatalf("MarkInterrupt wrote %s before the turn settled", errID)
	}
	requireNoStoppedRow(t, st, threadID)
}

// The provider aborts the turn the send opens: that turn gets the stop row
// when it settles, and the turn after it is not marked.
func TestStopOfAWaitingSendIsWrittenWhenItsTurnEndsInterrupted(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	router.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{})

	stopWaitingSend(t, router, st, "t1", 1)
	handleStopEvent(t, router, initEvent("t1"))
	if got := router.OpenTurnIndex("t1"); got != 1 {
		t.Fatalf("open turn = %d, want 1", got)
	}
	requireNoStoppedRow(t, st, "t1")
	handleStopEvent(t, router, turnCompleteEvent("t1", "interrupted"))
	requireOneStoppedRow(t, st, "t1", 1)

	router.RegisterPendingSendWithExpectation("t1", "user:2", 2, PendingSendExpectation{})
	handleStopEvent(t, router, initEvent("t1"))
	handleStopEvent(t, router, turnCompleteEvent("t1", "interrupted"))
	requireOneStoppedRow(t, st, "t1", 1)
}

// The interrupt reached an idle provider and the send ran to a normal end:
// its turn was not stopped.
func TestStopOfAWaitingSendWritesNothingWhenItsTurnEndsNormally(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	router.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{})

	stopWaitingSend(t, router, st, "t1", 1)
	handleStopEvent(t, router, initEvent("t1"))
	handleStopEvent(t, router, turnCompleteEvent("t1", "end_turn"))
	requireNoStoppedRow(t, st, "t1")
}

// The provider retired the send unrun: no turn ran, so nothing was stopped,
// and a later send that takes the same turn index and ends interrupted is
// not read as this Stop's turn.
func TestStopOfAWaitingSendWritesNothingWhenTheSendIsRetiredUnrun(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	registerCommandPendingSend(router, "t1", "user:1", testCommandUUID, 1)

	stopWaitingSend(t, router, st, "t1", 1)
	handleStopEvent(t, router, commandLifecycleEvent("t1", testCommandUUID, provider.CommandCancelled))
	if router.HasPendingSend("t1", "user:1") {
		t.Fatal("the cancelled send is still pending")
	}
	requireNoStoppedRow(t, st, "t1")

	router.RegisterPendingSendWithExpectation("t1", "user:1:next", 1, PendingSendExpectation{})
	handleStopEvent(t, router, initEvent("t1"))
	handleStopEvent(t, router, turnCompleteEvent("t1", "interrupted"))
	requireNoStoppedRow(t, st, "t1")
}

// Stopping again before the turn opens holds nothing more.
func TestRepeatedStopsOfAWaitingSendWriteOneRow(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	router.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{})

	stopWaitingSend(t, router, st, "t1", 1)
	stopWaitingSend(t, router, st, "t1", 1)
	handleStopEvent(t, router, initEvent("t1"))
	handleStopEvent(t, router, turnCompleteEvent("t1", "interrupted"))
	requireOneStoppedRow(t, st, "t1", 1)
}

// A Stop of the turn once it is open writes at once, and the settlement
// does not write the earlier held one.
func TestStopOfTheOpenedTurnReplacesTheHeldStop(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	router.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{})

	stopWaitingSend(t, router, st, "t1", 1)
	handleStopEvent(t, router, initEvent("t1"))
	sample := router.SampleInterrupt("t1")
	if sample.Turn != 1 {
		t.Fatalf("open turn = %d, want 1", sample.Turn)
	}
	errID, err := router.MarkInterrupt("t1", sample, router.MarkFlushSendsInterrupted("t1", sample.Turn), StoppedByUser)
	if err != nil {
		t.Fatalf("MarkInterrupt: %v", err)
	}
	requireStoppedByUserRow(t, st, "t1", errID, 1)
	handleStopEvent(t, router, turnCompleteEvent("t1", "interrupted"))
	requireOneStoppedRow(t, st, "t1", 1)
}

// The waiting send's turn opened during the ack wait: the stop is still
// held for its settlement, which decides.
func TestStopOfAWaitingSendOpenedDuringTheAck(t *testing.T) {
	for _, tc := range []struct {
		stopReason string
		want       int
	}{{"interrupted", 1}, {"end_turn", 0}} {
		t.Run(tc.stopReason, func(t *testing.T) {
			router, st, _ := newTestRouter(t)
			createTestThread(t, st, "t1")
			router.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{})

			sample := router.SampleInterrupt("t1")
			tok := router.MarkFlushSendsInterrupted("t1", sample.Turn)
			handleStopEvent(t, router, initEvent("t1"))
			if errID, err := router.MarkInterrupt("t1", sample, tok, StoppedByUser); err != nil || errID != "" {
				t.Fatalf("MarkInterrupt = %q, %v; want the stop held", errID, err)
			}
			handleStopEvent(t, router, turnCompleteEvent("t1", tc.stopReason))
			if got := len(stoppedRows(t, st, "t1")); got != tc.want {
				t.Fatalf("stop rows = %d, want %d", got, tc.want)
			}
		})
	}
}

// The waiting send's turn opened and settled during the ack wait: the Stop
// reads how it ended.
func TestStopOfAWaitingSendWhoseTurnSettledDuringTheAck(t *testing.T) {
	for _, tc := range []struct {
		stopReason string
		want       int
	}{{"interrupted", 1}, {"end_turn", 0}} {
		t.Run(tc.stopReason, func(t *testing.T) {
			router, st, _ := newTestRouter(t)
			createTestThread(t, st, "t1")
			router.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{})

			sample := router.SampleInterrupt("t1")
			tok := router.MarkFlushSendsInterrupted("t1", sample.Turn)
			handleStopEvent(t, router, initEvent("t1"))
			handleStopEvent(t, router, turnCompleteEvent("t1", tc.stopReason))
			if _, err := router.MarkInterrupt("t1", sample, tok, StoppedByUser); err != nil {
				t.Fatalf("MarkInterrupt: %v", err)
			}
			if got := len(stoppedRows(t, st, "t1")); got != tc.want {
				t.Fatalf("stop rows = %d, want %d", got, tc.want)
			}
		})
	}
}

// A soft round close leaves the turn's result to come: the result decides.
func TestStopOfAWaitingSendWaitsOutASoftClose(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	router.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{})

	stopWaitingSend(t, router, st, "t1", 1)
	handleStopEvent(t, router, initEvent("t1"))
	handleStopEvent(t, router, provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: &provider.SoftRoundCloseMeta{StopReason: "end_turn"},
		Timestamp:    time.Now(),
	})
	requireNoStoppedRow(t, st, "t1")
	handleStopEvent(t, router, turnCompleteEvent("t1", "interrupted"))
	requireOneStoppedRow(t, st, "t1", 1)
}

// The waiting send's echo opens its turn before any init does: the stop
// is the turn's own once it ends interrupted, and the init that follows
// adds nothing.
func TestStopOfAWaitingSendOpenedByItsEcho(t *testing.T) {
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

	stopWaitingSend(t, router, st, "t1", 1)
	handleStopEvent(t, router, provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: "t1", Content: "just sent",
		Meta: json.RawMessage(`{"provider_item_id":"echo-1"}`), Timestamp: time.Now(),
	})
	if got := router.OpenTurnIndex("t1"); got != 1 {
		t.Fatalf("open turn after the echo = %d, want 1", got)
	}
	handleStopEvent(t, router, initEvent("t1"))
	handleStopEvent(t, router, turnCompleteEvent("t1", "interrupted"))
	requireOneStoppedRow(t, st, "t1", 1)
}

// A Stop held while the waiting send's turn start is in progress resolves
// at that turn's settlement.
func TestStopOfAWaitingSendWhoseStartIsInProgress(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	router.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{})

	sample := router.SampleInterrupt("t1")
	tok := router.MarkFlushSendsInterrupted("t1", sample.Turn)
	held := holdTurnStart(t, router, initEvent("t1"), false)
	if errID, err := router.MarkInterrupt("t1", sample, tok, StoppedByUser); err != nil || errID != "" {
		t.Fatalf("MarkInterrupt = %q, %v; want the stop held", errID, err)
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
	handleStopEvent(t, router, turnCompleteEvent("t1", "interrupted"))
	requireOneStoppedRow(t, st, "t1", 1)
}

// The send was retired unrun during the ack wait: the Stop holds nothing,
// so a later send's turn at the same index is not read as stopped.
func TestStopOfASendRetiredDuringTheAckHoldsNothing(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	registerCommandPendingSend(router, "t1", "user:1", testCommandUUID, 1)

	sample := router.SampleInterrupt("t1")
	tok := router.MarkFlushSendsInterrupted("t1", sample.Turn)
	handleStopEvent(t, router, commandLifecycleEvent("t1", testCommandUUID, provider.CommandCancelled))
	if errID, err := router.MarkInterrupt("t1", sample, tok, StoppedByUser); err != nil || errID != "" {
		t.Fatalf("MarkInterrupt = %q, %v; want nothing written", errID, err)
	}
	router.RegisterPendingSendWithExpectation("t1", "user:1:next", 1, PendingSendExpectation{})
	handleStopEvent(t, router, initEvent("t1"))
	handleStopEvent(t, router, turnCompleteEvent("t1", "interrupted"))
	requireNoStoppedRow(t, st, "t1")
}

// The waiting send's turn settled, and a later turn after it, before the
// Stop was marked: the Stop reads the cut turn's row.
func TestStopOfAWaitingSendWhoseTurnSettledBeforeTheLatest(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	router.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{})

	sample := router.SampleInterrupt("t1")
	tok := router.MarkFlushSendsInterrupted("t1", sample.Turn)
	handleStopEvent(t, router, initEvent("t1"))
	handleStopEvent(t, router, turnCompleteEvent("t1", "interrupted"))
	router.RegisterPendingSendWithExpectation("t1", "user:2", 2, PendingSendExpectation{})
	handleStopEvent(t, router, initEvent("t1"))
	handleStopEvent(t, router, turnCompleteEvent("t1", "end_turn"))
	if _, err := router.MarkInterrupt("t1", sample, tok, StoppedByUser); err != nil {
		t.Fatalf("MarkInterrupt: %v", err)
	}
	requireOneStoppedRow(t, st, "t1", 1)
}

// A replacement session reuses turn indexes: a Stop held for the previous
// process's turn does not mark the replacement's.
func TestHeldStopDoesNotOutliveItsSession(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	router.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{})

	stopWaitingSend(t, router, st, "t1", 1)
	router.MarkThreadActive("t1", "session-b")
	router.RegisterPendingSendWithExpectation("t1", "user:1:again", 1, PendingSendExpectation{})
	handleStopEvent(t, router, initEvent("t1"))
	handleStopEvent(t, router, turnCompleteEvent("t1", "interrupted"))
	requireNoStoppedRow(t, st, "t1")
}

// A steer the provider never confirmed is no waiting send: it joined a
// turn that has since ended and opens none. The direct send registered
// after it is the one an interrupt cuts.
func TestSampleInterrupt_UnconfirmedSteerIsNotAWaitingSend(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 1)
	router.RegisterPendingSteerSendWithExpectation("t1", "user:1:steer:1", 1, PendingSendExpectation{ByClientID: true})
	handleStopEvent(t, router, turnCompleteEvent("t1", "interrupted"))

	if sample := router.SampleInterrupt("t1"); sample.Turn != -1 || sample.CutTurn() != -1 {
		t.Fatalf("sample with only the steer = open %d cut %d, want nothing to cut", sample.Turn, sample.CutTurn())
	}
	router.RegisterPendingSendWithExpectation("t1", "user:2", 2, PendingSendExpectation{ByClientID: true})
	if sample := router.SampleInterrupt("t1"); sample.CutTurn() != 2 {
		t.Fatalf("cut turn = %d, want the direct send's turn 2", sample.CutTurn())
	}
}

// A Stop that lands while a tick (a loop or watch wake) waits for its
// turn is held for that turn as a person's send's is: the turn ending
// interrupted gets one stop row. The interrupted tick is the user's to
// read, not quiet, and its delivery is reported once, into its own turn.
func TestMarkInterrupt_WaitingTickIsStoppedOnceAndNotQuiet(t *testing.T) {
	const uuid = "uuid-tick-1"
	router, st, emissions, _ := quietTurnRouter(t, true)
	var delivered []SendDelivery
	router.SetSendDeliveredHook(func(_ string, d SendDelivery) {
		if !d.AwaitingTurn {
			delivered = append(delivered, d)
		}
	})
	runTurn(t, router, time.Now().UnixMilli()-2_000, time.Now().UnixMilli()-1_000, normalTurnCompleteMeta())
	before, err := st.GetThread("t1")
	if err != nil {
		t.Fatal(err)
	}
	earlier := len(completedEvents(emissions))
	now := time.Now().UnixMilli()
	if err := router.PersistItem(store.Item{
		ID: "user:1", ThreadID: "t1", TurnIndex: 1, Kind: "user_text", Role: "user",
		Status: "completed", Summary: "sent", CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatalf("persist tick row: %v", err)
	}
	router.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{ProviderItemID: uuid, Members: []string{"agent-wake:w1:1"}})

	stopWaitingSend(t, router, st, "t1", 1)

	completeAt := time.Now().UnixMilli() + 10_000
	handleAll(t, router,
		commandLifecycleEvent("t1", uuid, provider.CommandStarted),
		openerInit(t, provider.TurnOpenerCommand),
		echoUUID(uuid),
		provider.ProviderEvent{
			Kind: provider.EventTurnComplete, ThreadID: "t1", Timestamp: millis(completeAt),
			TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn", Aborted: true},
		})

	requireOneStoppedRow(t, st, "t1", 1)
	if len(delivered) != 1 || delivered[0].TurnIndex != 1 || delivered[0].UserItemID != "user:1" {
		t.Fatalf("deliveries = %+v, want one into turn 1", delivered)
	}
	if events := completedEvents(emissions)[earlier:]; len(events) != 1 || events[0].Quiet {
		t.Fatalf("turn_completed = %+v, want one event, not quiet", events)
	}
	thread, err := st.GetThread("t1")
	if err != nil {
		t.Fatal(err)
	}
	if thread.LastReadAt == nil || before.LastReadAt == nil || *thread.LastReadAt != *before.LastReadAt {
		t.Fatalf("last_read_at = %v, want it left at %v: the stopped tick is the user's to read", thread.LastReadAt, before.LastReadAt)
	}
}
