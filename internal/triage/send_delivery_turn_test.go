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

// Which turn a queued send is delivered into, across the ways the provider
// takes it: drained into the running turn, picked up as a turn of its own,
// or taken at a turn boundary and answered by the turn that opens next.

// turnDelivery is one delivery with its turn named: the send ids and the
// turn they were delivered into.
type turnDelivery struct {
	sendIDs []string
	turn    int
}

func recordDeliveries(router *Router) *deliveryLog {
	log := &deliveryLog{}
	router.SetSendDeliveredHook(log.record)
	return log
}

// wantDelivered checks the deliveries with a named turn, in order. Rebinds
// are a different report and fail the check.
func wantDelivered(t *testing.T, log *deliveryLog, want ...turnDelivery) {
	t.Helper()
	seen := log.snapshot()
	if !slices.EqualFunc(seen, want, func(got SendDelivery, w turnDelivery) bool {
		return !got.Rebind && got.TurnIndex == w.turn && slices.Equal(got.SendIDs, w.sendIDs)
	}) {
		t.Fatalf("deliveries = %+v, want %+v", seen, want)
	}
}

// wantAwaiting checks the sends reported taken in before their turn opened,
// in order.
func wantAwaiting(t *testing.T, log *deliveryLog, sendIDs ...[]string) {
	t.Helper()
	awaiting := log.awaitingSnapshot()
	if !slices.EqualFunc(awaiting, sendIDs, func(got SendDelivery, want []string) bool {
		return slices.Equal(got.SendIDs, want)
	}) {
		t.Fatalf("deliveries awaiting a turn = %+v, want %v", awaiting, sendIDs)
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

// A message the provider picks up after the turn it was queued in waits
// through that turn's end, is claimed by the pickup's turn start, and is
// delivered into that turn.
func TestSendPickedUpAfterItsTurnIsDeliveredIntoTheTurnItOpens(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	log := recordDeliveries(router)
	startTurnZero(t, router)
	row := queueDuringTurnZero(t, router, 1, "send-1", "echo-1", 1)
	wantPendingSend(t, router, row.ID, "while turn 0 runs", 0, PendingSendWaiting)

	endOpenTurn(t, router)
	wantPendingSend(t, router, row.ID, "after turn 0 ended without it", 0, PendingSendWaiting)

	if err := router.Handle(openerInit(t, provider.TurnOpenerCommand)); err != nil {
		t.Fatalf("pickup init: %v", err)
	}
	wantPendingSend(t, router, row.ID, "after the pickup init", 1, PendingSendClaimed)
	wantDelivered(t, log)

	if err := router.Handle(echoOf(row, "echo-1")); err != nil {
		t.Fatalf("echo: %v", err)
	}
	wantPendingSend(t, router, row.ID, "after the echo", 0, PendingSendNone)
	wantDelivered(t, log, turnDelivery{sendIDs: []string{"send-1"}, turn: 1})
}

// A message the provider drains into the running turn is delivered there.
func TestSendDrainedIntoTheRunningTurnIsDeliveredThere(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	log := recordDeliveries(router)
	startTurnZero(t, router)
	row := queueDuringTurnZero(t, router, 1, "send-1", "echo-1", 1)

	if err := router.Handle(echoOf(row, "echo-1")); err != nil {
		t.Fatalf("echo: %v", err)
	}
	wantPendingSend(t, router, row.ID, "after the drain", 0, PendingSendNone)
	wantDelivered(t, log, turnDelivery{sendIDs: []string{"send-1"}, turn: 0})
}

// A message the provider takes at a turn boundary is delivered into the turn
// that opens next, and is reported when that turn opens: until then it is
// reported only as taken in, and no reserved turn is named for it.
func TestSendConfirmedAtABoundaryIsDeliveredIntoTheTurnThatOpensNext(t *testing.T) {
	t.Run("another send opens the turn", func(t *testing.T) {
		router, st, _ := newTestRouter(t)
		createTestThread(t, st, "t1")
		log := recordDeliveries(router)
		startTurnZero(t, router)
		first := queueDuringTurnZero(t, router, 1, "send-1", "echo-1", 1)
		second := queueDuringTurnZero(t, router, 2, "send-2", "echo-2", 2)
		endOpenTurn(t, router)

		if err := router.Handle(echoOf(first, "echo-1")); err != nil {
			t.Fatalf("first echo: %v", err)
		}
		wantDelivered(t, log)
		wantAwaiting(t, log, []string{"send-1"})

		if err := router.Handle(openerInit(t, provider.TurnOpenerCommand)); err != nil {
			t.Fatalf("init: %v", err)
		}
		wantDelivered(t, log, turnDelivery{sendIDs: []string{"send-1"}, turn: 2})
		wantPendingSend(t, router, first.ID, "after the turn opened", 0, PendingSendNone)

		if err := router.Handle(echoOf(second, "echo-2")); err != nil {
			t.Fatalf("second echo: %v", err)
		}
		wantDelivered(t, log,
			turnDelivery{sendIDs: []string{"send-1"}, turn: 2},
			turnDelivery{sendIDs: []string{"send-2"}, turn: 2})
	})
	t.Run("the provider opens the turn", func(t *testing.T) {
		router, st, _ := newTestRouter(t)
		createTestThread(t, st, "t1")
		log := recordDeliveries(router)
		startTurnZero(t, router)
		row := queueDuringTurnZero(t, router, 1, "send-1", "echo-1", 1)
		endOpenTurn(t, router)

		if err := router.Handle(echoOf(row, "echo-1")); err != nil {
			t.Fatalf("echo: %v", err)
		}
		wantDelivered(t, log)
		wantAwaiting(t, log, []string{"send-1"})
		if err := router.Handle(openerInit(t, provider.TurnOpenerCommand)); err != nil {
			t.Fatalf("init: %v", err)
		}
		open, ok := router.openTurnIndex("t1")
		if !ok {
			t.Fatal("the init opened no turn")
		}
		wantDelivered(t, log, turnDelivery{sendIDs: []string{"send-1"}, turn: open})
	})
	// More sends than the merge ledger remembers: the fold declines, and
	// every member is still delivered into the one turn that ran.
	t.Run("more sends than a fold can prove", func(t *testing.T) {
		router, st, _ := newTestRouter(t)
		createTestThread(t, st, "t1")
		log := recordDeliveries(router)
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
		wantDelivered(t, log)
		if err := router.Handle(openerInit(t, provider.TurnOpenerCommand)); err != nil {
			t.Fatalf("init: %v", err)
		}
		if err := router.Handle(echoOf(rows[sends-1], fmt.Sprintf("echo-%d", sends))); err != nil {
			t.Fatalf("last echo: %v", err)
		}
		want := make([]turnDelivery, 0, sends)
		for n := 1; n <= sends; n++ {
			want = append(want, turnDelivery{sendIDs: []string{fmt.Sprintf("send-%d", n)}, turn: sends})
		}
		wantDelivered(t, log, want...)
	})
}

// A confirmation that arrives after its turn closed names the turn that
// took the send, not the turn a later waiting send reserved.
func TestLateConfirmationDeliversIntoTheTurnThatTookTheSend(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	log := recordDeliveries(router)
	startTurnZero(t, router)
	first := queueDuringTurnZero(t, router, 1, "send-1", "echo-1", 1)
	endOpenTurn(t, router)
	// The CLI's order for a turn it opens for a message: started, init.
	router.noteSendStarted("t1", "", "echo-1")
	if err := router.Handle(openerInit(t, provider.TurnOpenerCommand)); err != nil {
		t.Fatalf("init: %v", err)
	}
	second := queueDuringTurn(t, router, 1, 1, "send-2", "echo-2", 2)
	endOpenTurn(t, router)
	// `started` is the first delivery signal and decides.
	wantDelivered(t, log, turnDelivery{sendIDs: []string{"send-1"}, turn: 1})

	t.Run("nothing open", func(t *testing.T) {
		// The confirmation as the first delivery signal, which it is for
		// a provider whose `started` did not reach the router.
		consumed := pendingSendFor(t, router, first.ID)
		consumed.DeliveryReported = false
		probe := recordDeliveries(router)
		defer router.SetSendDeliveredHook(log.record)
		router.reportSendsDelivered("t1", "", []*pendingSend{&consumed})
		wantDelivered(t, probe, turnDelivery{sendIDs: []string{"send-1"}, turn: 1})
		wantAwaiting(t, probe)
	})
	t.Run("the waiting send's turn is open", func(t *testing.T) {
		router.noteSendStarted("t1", "", "echo-2")
		if err := router.Handle(openerInit(t, provider.TurnOpenerCommand)); err != nil {
			t.Fatalf("second init: %v", err)
		}
		wantPendingSend(t, router, second.ID, "the waiting send", 2, PendingSendClaimed)
		if err := router.Handle(echoOf(first, "echo-1")); err != nil {
			t.Fatalf("late echo: %v", err)
		}
		wantDelivered(t, log,
			turnDelivery{sendIDs: []string{"send-1"}, turn: 1},
			turnDelivery{sendIDs: []string{"send-2"}, turn: 2})
	})
}

// A turn that took a send the provider had not started (a turn the provider
// opened on its own) is not where the send is delivered when its echo then
// arrives in a later turn.
func TestSendTakenByAProviderStartedTurnIsDeliveredWhereItIsConfirmed(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	log := recordDeliveries(router)
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
	wantDelivered(t, log, turnDelivery{sendIDs: []string{"send-1"}, turn: open})
}

// A send the model started reading inside a turn is delivered into that
// turn, whenever its confirmation arrives.
func TestLateConfirmationDeliversIntoTheTurnTheSendStartedIn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	log := recordDeliveries(router)
	startTurnZero(t, router)
	first := queueDuringTurnZero(t, router, 1, "send-1", "echo-1", 1)
	queueDuringTurnZero(t, router, 2, "send-2", "echo-2", 2)
	router.noteSendStarted("t1", "", "echo-1")
	endOpenTurn(t, router)

	if err := router.Handle(echoOf(first, "echo-1")); err != nil {
		t.Fatalf("late echo: %v", err)
	}
	wantDelivered(t, log, turnDelivery{sendIDs: []string{"send-1"}, turn: 0})
}

// A direct send drained into a running turn opens its own turn when it is
// consumed, and the rest of the loop runs there. Its sender wrote the row at
// that turn and binds to it there, so the hook, which reports queued sends,
// names nothing.
func TestDirectSendDrainedMidTurnOpensItsOwnTurn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	log := recordDeliveries(router)
	startTurnZero(t, router)
	now := time.Now().UnixMilli()
	if err := router.PersistItem(store.Item{
		ID: "user:1", ThreadID: "t1", TurnIndex: 1, Kind: "user_text", Role: "user", Status: "completed",
		Summary: "sent", Meta: `{"sendId":"send-direct"}`, CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatalf("persist send row: %v", err)
	}
	router.RegisterPendingSendWithExpectation("t1", "user:1", 1, PendingSendExpectation{ProviderItemID: "echo-direct"})
	router.noteSendStarted("t1", "", "echo-direct")

	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventUserText, ThreadID: "t1", Content: "sent", Meta: json.RawMessage(`{"provider_item_id":"echo-direct"}`), Timestamp: time.Now()}); err != nil {
		t.Fatalf("echo: %v", err)
	}
	if open, ok := router.openTurnIndex("t1"); !ok || open != 1 {
		t.Fatalf("open turn = %d/%v, want the send's own turn 1", open, ok)
	}
	if seen, awaiting := log.snapshot(), log.awaitingSnapshot(); len(seen) != 0 || len(awaiting) != 0 {
		t.Fatalf("deliveries = %+v awaiting %+v, want none for a direct send", seen, awaiting)
	}
}

// A confirmation that is retried keeps the turn its first attempt named: the
// first delivery is the only one, whatever the row carries by the retry.
func TestRetriedConfirmationKeepsItsDelivery(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	log := recordDeliveries(router)
	startTurnZero(t, router)
	row := queueDuringTurnZero(t, router, 1, "send-1", "echo-1", 1)
	consumed := pendingSend{AOItemID: row.ID, TurnIndex: 1, QuietItem: &row, WrittenDuringTurn: 0}

	router.reportSendsDelivered("t1", "", []*pendingSend{&consumed})
	endOpenTurn(t, router)
	if err := router.Handle(openerInit(t, provider.TurnOpenerCommand)); err != nil {
		t.Fatalf("init: %v", err)
	}
	widened := row
	widened.Meta = `{"sendId":"send-0","joinedSendIds":["send-0","send-1"]}`
	if _, err := st.UpsertItem(widened, nil); err != nil {
		t.Fatalf("widen the row: %v", err)
	}
	router.reportSendsDelivered("t1", "", []*pendingSend{&consumed})
	wantDelivered(t, log, turnDelivery{sendIDs: []string{"send-1"}, turn: 0})
}

// A boundary send whose confirmation is retried before its turn opens is
// delivered once, at the turn start; a retry after the turn start reports
// nothing more.
func TestRetriedBoundaryConfirmationIsDeliveredAtTheTurnStart(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	log := recordDeliveries(router)
	startTurnZero(t, router)
	row := queueDuringTurnZero(t, router, 1, "send-1", "echo-1", 1)
	endOpenTurn(t, router)
	consumed := pendingSendFor(t, router, row.ID)
	consumed.EchoConsumed = true

	router.reportSendsDelivered("t1", "", []*pendingSend{&consumed})
	widened := row
	widened.Meta = `{"sendId":"send-0","joinedSendIds":["send-0","send-1"]}`
	if _, err := st.UpsertItem(widened, nil); err != nil {
		t.Fatalf("widen the row: %v", err)
	}
	router.reportSendsDelivered("t1", "", []*pendingSend{&consumed})
	wantDelivered(t, log)
	wantAwaiting(t, log, []string{"send-1"})

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
	wantDelivered(t, log, turnDelivery{sendIDs: []string{"send-1"}, turn: open})

	retried := pendingSendFor(t, router, row.ID)
	endOpenTurn(t, router)
	router.reportSendsDelivered("t1", "", []*pendingSend{&retried})
	wantDelivered(t, log, turnDelivery{sendIDs: []string{"send-1"}, turn: open})
	wantAwaiting(t, log, []string{"send-1"})
}

// A queue-boundary merge delivers every member into the survivor's turn,
// including a member whose own echo never arrived.
func TestMergedSendsAreDeliveredIntoTheSurvivorsTurn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	log := recordDeliveries(router)
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
	turns := map[string][]int{}
	for _, delivery := range log.snapshot() {
		if delivery.Rebind {
			t.Fatalf("a member's delivery was rebound: %+v", delivery)
		}
		for _, id := range delivery.SendIDs {
			turns[id] = append(turns[id], delivery.TurnIndex)
		}
	}
	for _, id := range []string{"send-a", "send-b"} {
		if !slices.Equal(turns[id], []int{open}) {
			t.Fatalf("%s delivered into %v, want once into the survivor's turn %d (all: %+v)", id, turns[id], open, log.snapshot())
		}
	}
	wantPendingSend(t, router, "user:0:flush:1", "the absorbed member", 0, PendingSendNone)
	wantPendingSend(t, router, "user:0:flush:2", "the survivor", 0, PendingSendNone)
}

// A row with no send id names no one to tell.
func TestConfirmedSendWithoutASendIDIsNotDelivered(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	log := recordDeliveries(router)
	settleTurnZero(t, router)
	persistSendRow(t, router, "user:1", 1)
	if err := router.Handle(openerInit(t, provider.TurnOpenerCommand)); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventUserText, ThreadID: "t1", Content: "sent", Meta: json.RawMessage(`{"provider_item_id":"echo-direct"}`), Timestamp: time.Now()}); err != nil {
		t.Fatalf("echo: %v", err)
	}
	wantDelivered(t, log)
	wantAwaiting(t, log)
}

// A reader that finds no pending entry reads state the delivered hook has
// already written: PendingSendTurn waits for a confirmation in progress.
func TestPendingSendTurnWaitsForTheDeliveredHook(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	startTurnZero(t, router)
	row := queueDuringTurnZero(t, router, 1, "send-1", "echo-1", 1)

	inHook, release := make(chan struct{}), make(chan struct{})
	router.SetSendDeliveredHook(func(string, SendDelivery) {
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
		t.Fatalf("PendingSendTurn returned state %d while the delivered hook was still running", state)
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

// PendingSendTurn reports a send claimed only by a turn start that ran it: a
// turn that took the send's index while its `queued` ack is on record and
// before its `started` was opened by the provider on its own, racing it.
func TestPendingSendTurnClaimsOnlyATurnThatRanTheSend(t *testing.T) {
	t.Run("no lifecycle acks", func(t *testing.T) {
		router, st, _ := newTestRouter(t)
		createTestThread(t, st, "t1")
		startTurnZero(t, router)
		row := queueDuringTurnZero(t, router, 1, "send-1", "echo-1", 1)
		wantPendingSend(t, router, row.ID, "before any turn start took it", 0, PendingSendWaiting)
		endOpenTurn(t, router)
		if err := router.Handle(provider.ProviderEvent{Kind: provider.EventInit, ThreadID: "t1", Timestamp: time.Now()}); err != nil {
			t.Fatalf("init: %v", err)
		}
		wantPendingSend(t, router, row.ID, "after the turn start took it", 1, PendingSendClaimed)
		if err := router.Handle(echoOf(row, "echo-1")); err != nil {
			t.Fatalf("echo: %v", err)
		}
		wantPendingSend(t, router, row.ID, "after its echo", 0, PendingSendNone)
	})
	t.Run("queued ack and no started", func(t *testing.T) {
		router, st, _ := newTestRouter(t)
		createTestThread(t, st, "t1")
		startTurnZero(t, router)
		row := queueDuringTurnZero(t, router, 1, "send-1", "echo-1", 1)
		if err := router.Handle(commandLifecycleEvent("t1", "echo-1", provider.CommandQueued)); err != nil {
			t.Fatalf("queued: %v", err)
		}
		endOpenTurn(t, router)
		if err := router.Handle(provider.ProviderEvent{Kind: provider.EventInit, ThreadID: "t1", Timestamp: time.Now()}); err != nil {
			t.Fatalf("init: %v", err)
		}
		if open, ok := router.openTurnIndex("t1"); !ok || open != 1 {
			t.Fatalf("open turn = %d/%v, want the send's index 1", open, ok)
		}
		wantPendingSend(t, router, row.ID, "a turn start took its index before its started", 0, PendingSendWaiting)
		if err := router.Handle(echoOf(row, "echo-1")); err != nil {
			t.Fatalf("echo: %v", err)
		}
		wantPendingSend(t, router, row.ID, "after its echo", 0, PendingSendNone)
	})
}
