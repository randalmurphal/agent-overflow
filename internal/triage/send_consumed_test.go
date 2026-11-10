package triage

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

// consumedReport is one call of the consumed hook.
type consumedReport struct {
	sendIDs []string
	turn    int
}

func recordConsumed(router *Router) *[]consumedReport {
	var reports []consumedReport
	router.SetSendConsumedHook(func(_ string, sendIDs []string, answerTurn int) {
		reports = append(reports, consumedReport{sendIDs: slices.Clone(sendIDs), turn: answerTurn})
	})
	return &reports
}

func wantConsumed(t *testing.T, reports *[]consumedReport, want ...consumedReport) {
	t.Helper()
	if !slices.EqualFunc(*reports, want, func(a, b consumedReport) bool {
		return a.turn == b.turn && slices.Equal(a.sendIDs, b.sendIDs)
	}) {
		t.Fatalf("consumed reports = %+v, want %+v", *reports, want)
	}
}

func startTurnZero(t *testing.T, router *Router) {
	t.Helper()
	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0, Timestamp: time.Now()}); err != nil {
		t.Fatalf("turn 0 start: %v", err)
	}
}

func endOpenTurn(t *testing.T, router *Router) {
	t.Helper()
	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: normalTurnCompleteMeta(), Timestamp: time.Now()}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}
}

// queueDuringTurnZero registers a message queued while turn 0 runs: its row
// sits in turn 0 and it reserved responseTurn for a pickup after turn 0.
func queueDuringTurnZero(t *testing.T, router *Router, n int, sendID, echoID string, responseTurn int) store.Item {
	t.Helper()
	now := time.Now().UnixMilli()
	meta, err := json.Marshal(map[string]string{"sendId": sendID})
	if err != nil {
		t.Fatal(err)
	}
	row := store.Item{
		ID: "user:0:flush:" + string(rune('0'+n)), ThreadID: "t1", TurnIndex: 0, Kind: "user_text", Role: "user",
		Status: "completed", Summary: "queued " + sendID, Meta: string(meta), CreatedAt: now, UpdatedAt: now,
	}
	if err := router.PersistAndRegisterPendingQuietFlushSendWithExpectation("t1", "queue-"+sendID, row, responseTurn, now, PendingSendExpectation{ProviderItemID: echoID}); err != nil {
		t.Fatalf("register queued send: %v", err)
	}
	return row
}

func echoOf(row store.Item, echoID string) provider.ProviderEvent {
	return provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: "t1", Content: row.Summary,
		Meta: json.RawMessage(`{"provider_item_id":"` + echoID + `"}`), Timestamp: time.Now(),
	}
}

func wantPendingSend(t *testing.T, router *Router, itemID, when string, wantTurn int, wantState PendingSendState) {
	t.Helper()
	turn, state := router.PendingSendTurn("t1", itemID)
	if turn != wantTurn || state != wantState {
		t.Fatalf("%s: PendingSendTurn = turn %d state %d, want turn %d state %d", when, turn, state, wantTurn, wantState)
	}
}

// A message the provider picks up after the turn it was queued in waits
// through that turn's end, is claimed by the pickup's turn start, and is
// answered by that turn.
func TestSendPickedUpAfterItsTurnIsAnsweredByTheTurnItOpens(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	reports := recordConsumed(router)
	startTurnZero(t, router)
	row := queueDuringTurnZero(t, router, 1, "send-1", "echo-1", 1)
	wantPendingSend(t, router, row.ID, "while turn 0 runs", 0, PendingSendWaiting)

	endOpenTurn(t, router)
	wantPendingSend(t, router, row.ID, "after turn 0 ended without it", 0, PendingSendWaiting)

	if err := router.Handle(openerInit(t, provider.TurnOpenerCommand)); err != nil {
		t.Fatalf("pickup init: %v", err)
	}
	wantPendingSend(t, router, row.ID, "after the pickup init", 1, PendingSendClaimed)
	wantConsumed(t, reports)

	if err := router.Handle(echoOf(row, "echo-1")); err != nil {
		t.Fatalf("echo: %v", err)
	}
	wantPendingSend(t, router, row.ID, "after the echo", 0, PendingSendNone)
	wantConsumed(t, reports, consumedReport{sendIDs: []string{"send-1"}, turn: 1})
}

// A message the provider drains into the running turn is answered there.
func TestSendDrainedIntoTheRunningTurnIsAnsweredThere(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	reports := recordConsumed(router)
	startTurnZero(t, router)
	row := queueDuringTurnZero(t, router, 1, "send-1", "echo-1", 1)

	if err := router.Handle(echoOf(row, "echo-1")); err != nil {
		t.Fatalf("echo: %v", err)
	}
	wantPendingSend(t, router, row.ID, "after the drain", 0, PendingSendNone)
	wantConsumed(t, reports, consumedReport{sendIDs: []string{"send-1"}, turn: 0})
}

// A message the provider takes at a turn boundary, before the turn that
// answers it has started, is answered by the turn the next waiting send
// opens, and by its own reservation when nothing else waits.
func TestSendConsumedAtABoundaryIsAnsweredByTheNextTurn(t *testing.T) {
	t.Run("another send waits", func(t *testing.T) {
		router, st, _ := newTestRouter(t)
		createTestThread(t, st, "t1")
		reports := recordConsumed(router)
		startTurnZero(t, router)
		first := queueDuringTurnZero(t, router, 1, "send-1", "echo-1", 1)
		second := queueDuringTurnZero(t, router, 2, "send-2", "echo-2", 2)
		endOpenTurn(t, router)

		if err := router.Handle(echoOf(first, "echo-1")); err != nil {
			t.Fatalf("first echo: %v", err)
		}
		wantConsumed(t, reports, consumedReport{sendIDs: []string{"send-1"}, turn: 2})

		if err := router.Handle(openerInit(t, provider.TurnOpenerCommand)); err != nil {
			t.Fatalf("init: %v", err)
		}
		if open, ok := router.openTurnIndex("t1"); !ok || open != 2 {
			t.Fatalf("open turn = %d/%v, want turn 2, the one the report named", open, ok)
		}
		if err := router.Handle(echoOf(second, "echo-2")); err != nil {
			t.Fatalf("second echo: %v", err)
		}
		wantConsumed(t, reports,
			consumedReport{sendIDs: []string{"send-1"}, turn: 2},
			consumedReport{sendIDs: []string{"send-2"}, turn: 2})
	})
	t.Run("nothing else waits", func(t *testing.T) {
		router, st, _ := newTestRouter(t)
		createTestThread(t, st, "t1")
		reports := recordConsumed(router)
		startTurnZero(t, router)
		row := queueDuringTurnZero(t, router, 1, "send-1", "echo-1", 1)
		endOpenTurn(t, router)

		if err := router.Handle(echoOf(row, "echo-1")); err != nil {
			t.Fatalf("echo: %v", err)
		}
		wantConsumed(t, reports, consumedReport{sendIDs: []string{"send-1"}, turn: 1})
	})
}

// A queue-boundary merge answers every member in the survivor's turn: the
// survivor's report carries the send ids of the rows it absorbed, including
// a member whose own echo never arrived.
func TestMergedSendsAreAnsweredByTheSurvivorsTurn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	reports := recordConsumed(router)
	startTurnZero(t, router)
	mergeFoldSend(t, router, "t1", "user:0:flush:1", "uuid-a", "first queued", nil, usermessage.Meta{SendID: "send-a"}, 0, 1)
	mergeFoldSend(t, router, "t1", "user:0:flush:2", "uuid-b", "second queued", nil, usermessage.Meta{SendID: "send-b"}, 0, 2)
	endOpenTurn(t, router)

	if err := router.Handle(openerInit(t, provider.TurnOpenerCommand)); err != nil {
		t.Fatalf("init: %v", err)
	}
	open, ok := router.openTurnIndex("t1")
	if !ok {
		t.Fatal("the pickup init opened no turn")
	}
	if err := router.Handle(mergeEcho(t, "t1", "uuid-b", textBlocks("first queued", "second queued"), true)); err != nil {
		t.Fatalf("survivor echo: %v", err)
	}
	wantConsumed(t, reports, consumedReport{sendIDs: []string{"send-a", "send-b"}, turn: open})
	wantPendingSend(t, router, "user:0:flush:1", "the absorbed member", 0, PendingSendNone)
	wantPendingSend(t, router, "user:0:flush:2", "the survivor", 0, PendingSendNone)
}

// A row with no send id names no one to tell.
func TestConsumedSendWithoutASendIDIsNotReported(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	reports := recordConsumed(router)
	settleTurnZero(t, router)
	persistSendRow(t, router, "user:1", 1)
	if err := router.Handle(openerInit(t, provider.TurnOpenerCommand)); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventUserText, ThreadID: "t1", Content: "sent", Meta: json.RawMessage(`{"provider_item_id":"echo-direct"}`), Timestamp: time.Now()}); err != nil {
		t.Fatalf("echo: %v", err)
	}
	wantConsumed(t, reports)
}

// A reader that finds no pending entry reads state the consumed hook has
// already written: PendingSendTurn waits for a confirmation in progress.
func TestPendingSendTurnWaitsForTheConsumedHook(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	startTurnZero(t, router)
	row := queueDuringTurnZero(t, router, 1, "send-1", "echo-1", 1)

	inHook, release := make(chan struct{}), make(chan struct{})
	router.SetSendConsumedHook(func(string, []string, int) {
		close(inHook)
		<-release
	})
	echoed := make(chan error, 1)
	go func() { echoed <- router.Handle(echoOf(row, "echo-1")) }()
	<-inHook

	read := make(chan PendingSendState, 1)
	go func() {
		_, state := router.PendingSendTurn("t1", row.ID)
		read <- state
	}()
	select {
	case state := <-read:
		close(release)
		t.Fatalf("PendingSendTurn returned state %d while the consumed hook was still running", state)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-echoed; err != nil {
		t.Fatalf("echo: %v", err)
	}
	if state := <-read; state != PendingSendNone {
		t.Fatalf("state after the confirmation = %d, want none", state)
	}
}
