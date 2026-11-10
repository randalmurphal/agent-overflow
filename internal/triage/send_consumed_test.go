package triage

import (
	"encoding/json"
	"fmt"
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
	return queueDuringTurn(t, router, 0, n, sendID, echoID, responseTurn)
}

func queueDuringTurn(t *testing.T, router *Router, rowTurn, n int, sendID, echoID string, responseTurn int) store.Item {
	t.Helper()
	now := time.Now().UnixMilli()
	meta, err := json.Marshal(map[string]string{"sendId": sendID})
	if err != nil {
		t.Fatal(err)
	}
	row := store.Item{
		ID: fmt.Sprintf("user:%d:flush:%d", rowTurn, n), ThreadID: "t1", TurnIndex: rowTurn, Kind: "user_text", Role: "user",
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

// A message the provider takes at a turn boundary is answered by the turn
// that opens next, and is reported when that turn opens: until then it is
// still waiting, and no reserved turn is named for it.
func TestSendConsumedAtABoundaryIsAnsweredByTheTurnThatOpensNext(t *testing.T) {
	t.Run("another send opens the turn", func(t *testing.T) {
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
		wantConsumed(t, reports)
		wantPendingSend(t, router, first.ID, "consumed at the boundary", 0, PendingSendWaiting)

		if err := router.Handle(openerInit(t, provider.TurnOpenerCommand)); err != nil {
			t.Fatalf("init: %v", err)
		}
		wantConsumed(t, reports, consumedReport{sendIDs: []string{"send-1"}, turn: 2})
		wantPendingSend(t, router, first.ID, "after the turn opened", 0, PendingSendNone)

		if err := router.Handle(echoOf(second, "echo-2")); err != nil {
			t.Fatalf("second echo: %v", err)
		}
		wantConsumed(t, reports,
			consumedReport{sendIDs: []string{"send-1"}, turn: 2},
			consumedReport{sendIDs: []string{"send-2"}, turn: 2})
	})
	t.Run("the provider opens the turn", func(t *testing.T) {
		router, st, _ := newTestRouter(t)
		createTestThread(t, st, "t1")
		reports := recordConsumed(router)
		startTurnZero(t, router)
		row := queueDuringTurnZero(t, router, 1, "send-1", "echo-1", 1)
		endOpenTurn(t, router)

		if err := router.Handle(echoOf(row, "echo-1")); err != nil {
			t.Fatalf("echo: %v", err)
		}
		wantConsumed(t, reports)
		if err := router.Handle(openerInit(t, provider.TurnOpenerCommand)); err != nil {
			t.Fatalf("init: %v", err)
		}
		open, ok := router.openTurnIndex("t1")
		if !ok {
			t.Fatal("the init opened no turn")
		}
		wantConsumed(t, reports, consumedReport{sendIDs: []string{"send-1"}, turn: open})
	})
	// More sends than the merge ledger remembers: the fold declines, and
	// every member is still answered by the one turn that ran.
	t.Run("more sends than a fold can prove", func(t *testing.T) {
		router, st, _ := newTestRouter(t)
		createTestThread(t, st, "t1")
		reports := recordConsumed(router)
		startTurnZero(t, router)
		const sends = flushSendDigestHistory + 1
		rows := make([]store.Item, 0, sends)
		for n := 1; n <= sends; n++ {
			rows = append(rows, queueDuringTurnZero(t, router, n, fmt.Sprintf("send-%d", n), fmt.Sprintf("echo-%d", n), n))
		}
		endOpenTurn(t, router)
		for n := 1; n < sends; n++ {
			if err := router.Handle(echoOf(rows[n-1], fmt.Sprintf("echo-%d", n))); err != nil {
				t.Fatalf("echo %d: %v", n, err)
			}
		}
		wantConsumed(t, reports)
		if err := router.Handle(openerInit(t, provider.TurnOpenerCommand)); err != nil {
			t.Fatalf("init: %v", err)
		}
		if err := router.Handle(echoOf(rows[sends-1], fmt.Sprintf("echo-%d", sends))); err != nil {
			t.Fatalf("last echo: %v", err)
		}
		if len(*reports) != sends {
			t.Fatalf("%d reports, want one per send (%d): %+v", len(*reports), sends, *reports)
		}
		for i, report := range *reports {
			if report.turn != sends || !slices.Equal(report.sendIDs, []string{fmt.Sprintf("send-%d", i+1)}) {
				t.Fatalf("report %d = %+v, want send-%d answered by turn %d, the one that ran", i, report, i+1, sends)
			}
		}
	})
}

// A confirmation that arrives after its turn closed names the turn that
// took the send, not the turn a later waiting send reserved.
func TestLateConfirmationNamesTheTurnThatTookTheSend(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	reports := recordConsumed(router)
	startTurnZero(t, router)
	first := queueDuringTurnZero(t, router, 1, "send-1", "echo-1", 1)
	endOpenTurn(t, router)
	// The CLI's order for a turn it opens for a message: started, init.
	router.markPendingSendStarted("t1", "echo-1")
	if err := router.Handle(openerInit(t, provider.TurnOpenerCommand)); err != nil {
		t.Fatalf("init: %v", err)
	}
	second := queueDuringTurn(t, router, 1, 1, "send-2", "echo-2", 2)
	endOpenTurn(t, router)

	t.Run("nothing open", func(t *testing.T) {
		*reports = nil
		consumed := pendingSendFor(t, router, first.ID)
		router.reportSendConsumed("t1", &consumed)
		wantConsumed(t, reports, consumedReport{sendIDs: []string{"send-1"}, turn: 1})
	})
	t.Run("the waiting send's turn is open", func(t *testing.T) {
		*reports = nil
		router.markPendingSendStarted("t1", "echo-2")
		if err := router.Handle(openerInit(t, provider.TurnOpenerCommand)); err != nil {
			t.Fatalf("second init: %v", err)
		}
		wantPendingSend(t, router, second.ID, "the waiting send", 2, PendingSendClaimed)
		if err := router.Handle(echoOf(first, "echo-1")); err != nil {
			t.Fatalf("late echo: %v", err)
		}
		wantConsumed(t, reports, consumedReport{sendIDs: []string{"send-1"}, turn: 1})
	})
}

// pendingSendFor copies the registered entry for itemID.
func pendingSendFor(t *testing.T, router *Router, itemID string) pendingSend {
	t.Helper()
	router.mu.Lock()
	defer router.mu.Unlock()
	for _, entry := range router.pendingSendsLocked("t1") {
		if entry.AOItemID == itemID {
			return entry
		}
	}
	t.Fatalf("no pending send for %s", itemID)
	return pendingSend{}
}

// A turn that took a send the provider had not started (a turn the provider
// opened on its own) is not where the send is answered when its echo then
// arrives in a later turn.
func TestSendTakenByAProviderStartedTurnIsAnsweredWhereItIsConsumed(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	reports := recordConsumed(router)
	startTurnZero(t, router)
	row := queueDuringTurnZero(t, router, 1, "send-1", "echo-1", 1)
	endOpenTurn(t, router)
	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventInit, ThreadID: "t1", Timestamp: time.Now()}); err != nil {
		t.Fatalf("provider-started init: %v", err)
	}
	wantPendingSend(t, router, row.ID, "taken by the provider's turn", 1, PendingSendClaimed)
	endOpenTurn(t, router)

	if err := router.Handle(openerInit(t, provider.TurnOpenerCommand)); err != nil {
		t.Fatalf("the message's own init: %v", err)
	}
	open, ok := router.openTurnIndex("t1")
	if !ok || open == 1 {
		t.Fatalf("open turn = %d/%v, want a turn after the provider's turn 1", open, ok)
	}
	if err := router.Handle(echoOf(row, "echo-1")); err != nil {
		t.Fatalf("echo: %v", err)
	}
	wantConsumed(t, reports, consumedReport{sendIDs: []string{"send-1"}, turn: open})
}

// A send the model started reading inside a turn is answered by that turn
// when its confirmation arrives after the turn closed.
func TestLateConfirmationNamesTheTurnTheSendStartedIn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	reports := recordConsumed(router)
	startTurnZero(t, router)
	first := queueDuringTurnZero(t, router, 1, "send-1", "echo-1", 1)
	queueDuringTurnZero(t, router, 2, "send-2", "echo-2", 2)
	router.markPendingSendStarted("t1", "echo-1")
	endOpenTurn(t, router)

	if err := router.Handle(echoOf(first, "echo-1")); err != nil {
		t.Fatalf("late echo: %v", err)
	}
	wantConsumed(t, reports, consumedReport{sendIDs: []string{"send-1"}, turn: 0})
}

// A direct send drained into a running turn opens its own turn when it is
// consumed, and the rest of the loop runs there: that turn answers, not the
// one the model started reading it in.
func TestDirectSendDrainedMidTurnIsAnsweredByItsOwnTurn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	reports := recordConsumed(router)
	startTurnZero(t, router)
	now := time.Now().UnixMilli()
	if err := router.PersistItem(store.Item{
		ID: "user:1", ThreadID: "t1", TurnIndex: 1, Kind: "user_text", Role: "user", Status: "completed",
		Summary: "sent", Meta: `{"sendId":"send-direct"}`, CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatalf("persist send row: %v", err)
	}
	router.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{ProviderItemID: "echo-direct"})
	router.markPendingSendStarted("t1", "echo-direct")

	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventUserText, ThreadID: "t1", Content: "sent", Meta: json.RawMessage(`{"provider_item_id":"echo-direct"}`), Timestamp: time.Now()}); err != nil {
		t.Fatalf("echo: %v", err)
	}
	if open, ok := router.openTurnIndex("t1"); !ok || open != 1 {
		t.Fatalf("open turn = %d/%v, want the send's own turn 1", open, ok)
	}
	wantConsumed(t, reports, consumedReport{sendIDs: []string{"send-direct"}, turn: 1})
}

// A confirmation that is retried keeps the turn its first attempt named,
// and reports the send ids the row carries by then: a fold that succeeds
// only on the retry has widened it.
func TestRetriedConfirmationKeepsItsTurnAndReportsWidenedSendIDs(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	reports := recordConsumed(router)
	startTurnZero(t, router)
	row := queueDuringTurnZero(t, router, 1, "send-1", "echo-1", 1)
	consumed := pendingSend{AOItemID: row.ID, TurnIndex: 1, QuietItem: &row}

	router.reportSendConsumed("t1", &consumed)
	endOpenTurn(t, router)
	if err := router.Handle(openerInit(t, provider.TurnOpenerCommand)); err != nil {
		t.Fatalf("init: %v", err)
	}
	widened := row
	widened.Meta = `{"sendId":"send-0","joinedSendIds":["send-0","send-1"]}`
	if _, err := st.UpsertItem(widened, nil); err != nil {
		t.Fatalf("widen the row: %v", err)
	}
	router.reportSendConsumed("t1", &consumed)
	wantConsumed(t, reports,
		consumedReport{sendIDs: []string{"send-1"}, turn: 0},
		consumedReport{sendIDs: []string{"send-0", "send-1"}, turn: 0})
}

// A boundary send whose confirmation is retried before its turn opens is
// reported once, at the turn start, with the send ids of the retry; and a
// retry after the turn start reports that same turn.
func TestRetriedBoundaryConfirmationIsReportedAtTheTurnStart(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	reports := recordConsumed(router)
	startTurnZero(t, router)
	row := queueDuringTurnZero(t, router, 1, "send-1", "echo-1", 1)
	endOpenTurn(t, router)
	consumed := pendingSendFor(t, router, row.ID)
	consumed.EchoConsumed = true

	router.reportSendConsumed("t1", &consumed)
	widened := row
	widened.Meta = `{"sendId":"send-0","joinedSendIds":["send-0","send-1"]}`
	if _, err := st.UpsertItem(widened, nil); err != nil {
		t.Fatalf("widen the row: %v", err)
	}
	router.reportSendConsumed("t1", &consumed)
	wantConsumed(t, reports)

	// The failed confirmation put the entry back for its retry.
	router.mu.Lock()
	queue := router.pendingSendsLocked("t1")
	for i := range queue {
		if queue[i].AOItemID == row.ID {
			queue[i] = consumed
		}
	}
	router.mu.Unlock()
	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventInit, ThreadID: "t1", Timestamp: time.Now()}); err != nil {
		t.Fatalf("init: %v", err)
	}
	open, ok := router.openTurnIndex("t1")
	if !ok {
		t.Fatal("the init opened no turn")
	}
	wantConsumed(t, reports, consumedReport{sendIDs: []string{"send-0", "send-1"}, turn: open})

	retried := pendingSendFor(t, router, row.ID)
	endOpenTurn(t, router)
	router.reportSendConsumed("t1", &retried)
	wantConsumed(t, reports,
		consumedReport{sendIDs: []string{"send-0", "send-1"}, turn: open},
		consumedReport{sendIDs: []string{"send-0", "send-1"}, turn: open})
}

// A send consumed at a boundary reads as waiting until the hook has been
// told its turn: a reader never finds it consumed with its turn unwritten.
func TestBoundarySendWaitsUntilItsTurnIsReported(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	startTurnZero(t, router)
	row := queueDuringTurnZero(t, router, 1, "send-1", "echo-1", 1)
	endOpenTurn(t, router)

	inHook, release := make(chan struct{}), make(chan struct{})
	router.SetSendConsumedHook(func(string, []string, int) {
		close(inHook)
		<-release
	})
	if err := router.Handle(echoOf(row, "echo-1")); err != nil {
		t.Fatalf("echo: %v", err)
	}
	opened := make(chan error, 1)
	go func() { opened <- router.Handle(openerInit(t, provider.TurnOpenerCommand)) }()
	select {
	case <-inHook:
	case err := <-opened:
		t.Fatalf("the turn opened (err %v) without the hook being told about the boundary send", err)
	}
	wantPendingSend(t, router, row.ID, "while the hook is being told", 0, PendingSendWaiting)
	close(release)
	if err := <-opened; err != nil {
		t.Fatalf("init: %v", err)
	}
	wantPendingSend(t, router, row.ID, "after the hook returned", 0, PendingSendNone)
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
